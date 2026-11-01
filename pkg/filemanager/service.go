package filemanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/write"
)

// maxUploadBytes bounds one upload, so a single request cannot fill the volume
// host's memory.
const maxUploadBytes = 64 << 20

// Store reads the account's setting values (C-23).
type Store interface {
	SettingValues(ctx context.Context, scope, target string) (map[string]string, error)
}

// Preferences reads the account's file browser preferences (C-27).
type Preferences interface {
	Preferences(ctx context.Context, session identity.Session, identifier string) (map[string]string, error)
}

// Config carries what the data plane needs besides its cluster readers.
type Config struct {
	// Settings reads the exposed-volume setting.
	Settings Store
	// Preferences reads the account's view and initial path.
	Preferences Preferences
	// Volumes resolves the volume object of one request.
	Volumes Volumes
	// Roots locates the directory served for one volume.
	Roots Roots
	// Log records a failure the caller must not learn about; defaults to
	// discarding it.
	Log *slog.Logger
	// Now supplies the response clock; defaults to time.Now.
	Now func() time.Time
}

// Service is the product's own data plane (NG10). It serves files inside the
// volumes the calling account exposed, and nothing else: every request resolves
// its volume again, so the service keeps no index.
type Service struct {
	settings    Store
	preferences Preferences
	volumes     Volumes
	roots       Roots
	log         *slog.Logger
	now         func() time.Time
}

// New builds the data plane.
func New(config Config) *Service {
	service := &Service{
		settings:    config.Settings,
		preferences: config.Preferences,
		volumes:     config.Volumes,
		roots:       config.Roots,
		log:         config.Log,
		now:         config.Now,
	}
	if service.log == nil {
		service.log = slog.New(slog.DiscardHandler)
	}
	if service.now == nil {
		service.now = time.Now
	}
	return service
}

// Handler serves the data plane. It is mounted behind the account middleware,
// which puts the session on the request context; the paths stay outside
// /api/v1 because file access is not a control-plane resource API (NG10).
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /files/window", s.handleWindow)
	mux.HandleFunc("GET /files/{namespace}/{volume}", s.handleListing)
	mux.HandleFunc("GET /files/{namespace}/{volume}/content", s.handleDownload)
	mux.HandleFunc("PUT /files/{namespace}/{volume}/content", s.handleUpload)
	mux.HandleFunc("POST /files/{namespace}/{volume}/rename", s.handleRename)
	mux.HandleFunc("DELETE /files/{namespace}/{volume}/content", s.handleDelete)
	return mux
}

// ListedVolume is one volume the window may open.
type ListedVolume struct {
	// Namespace is the volume's namespace.
	Namespace string `json:"namespace"`
	// Name is the volume's name.
	Name string `json:"name"`
	// Owner is the account the volume belongs to (C-60).
	Owner string `json:"owner,omitempty"`
}

// Window is what the application window opens with (C-30): the account's
// preference values and the volumes it may browse.
type Window struct {
	// View is the browser view, list or grid.
	View string `json:"view"`
	// InitialPath is the path the window opens at.
	InitialPath string `json:"initialPath"`
	// Volumes are the volumes this account exposed and may open.
	Volumes []ListedVolume `json:"volumes"`
	// Problems lists the entries that could not be served.
	Problems []string `json:"problems,omitempty"`
}

// Entry is one file or directory below a volume path.
type Entry struct {
	// Name is the entry's base name.
	Name string `json:"name"`
	// Kind is file or directory.
	Kind string `json:"kind"`
	// Size is the file size in bytes; zero for a directory.
	Size int64 `json:"size"`
	// ModifiedAt is the entry's modification time.
	ModifiedAt time.Time `json:"modifiedAt"`
}

// Listing is one directory read.
type Listing struct {
	// Volume is the volume the listing belongs to.
	Volume string `json:"volume"`
	// Path is the directory path inside the volume.
	Path string `json:"path"`
	// Entries are the directory's contents.
	Entries []Entry `json:"entries"`
}

// handleWindow opens the application window.
func (s *Service) handleWindow(w http.ResponseWriter, r *http.Request) {
	session, failure := sessionOf(r)
	if failure != nil {
		controlplane.WriteError(w, failure)
		return
	}
	window := Window{View: DefaultView, InitialPath: "", Volumes: []ListedVolume{}}
	view, initialPath, problems := s.opening(r.Context(), session)
	window.View, window.InitialPath = view, initialPath
	window.Problems = append(window.Problems, problems...)
	refs, failure := s.exposedVolumes(r.Context(), session)
	if failure != nil {
		controlplane.WriteError(w, failure)
		return
	}
	for _, ref := range refs {
		volume, err := s.volumes.Volume(r.Context(), ref.Namespace, ref.Name)
		if isNotFound(err) {
			window.Problems = append(window.Problems, "卷 "+ref.String()+" 已不存在")
			continue
		}
		if err != nil {
			s.unavailable(w, "读取卷 "+ref.String()+" 失败", err)
			return
		}
		if !ownerAllows(session, volume) {
			continue
		}
		window.Volumes = append(window.Volumes, ListedVolume{
			Namespace: ref.Namespace, Name: ref.Name, Owner: volume.Spec.Owner,
		})
	}
	writeJSON(w, http.StatusOK, window)
}

// opening reads the window's view and initial path, falling back to the
// defaults for a value the browser state does not know.
func (s *Service) opening(ctx context.Context, session identity.Session) (string, string, []string) {
	view, initialPath := DefaultView, ""
	if s.preferences == nil {
		return view, initialPath, nil
	}
	values, err := s.preferences.Preferences(ctx, session, Identifier)
	if err != nil {
		s.log.Error("read the file browser preferences", "error", err)
		return view, initialPath, []string{"偏好暂不可读"}
	}
	if stored := values[PreferenceView]; stored == ViewList || stored == ViewGrid {
		view = stored
	}
	if stored := values[PreferenceInitialPath]; stored != "" {
		if cleaned, ok := cleanPath(stored); ok {
			initialPath = cleaned
		}
	}
	return view, initialPath, nil
}

// handleListing reads one directory of a volume.
func (s *Service) handleListing(w http.ResponseWriter, r *http.Request) {
	session, failure := sessionOf(r)
	if failure != nil {
		controlplane.WriteError(w, failure)
		return
	}
	root, relative, failure := s.openVolume(r.Context(), session, r)
	if failure != nil {
		controlplane.WriteError(w, failure)
		return
	}
	defer root.Close()
	directory, err := root.Open(insidePath(relative))
	if err != nil {
		controlplane.WriteError(w, s.readFailure(err))
		return
	}
	defer directory.Close()
	entries, err := directory.ReadDir(-1)
	if err != nil {
		controlplane.WriteError(w, s.readFailure(err))
		return
	}
	listing := Listing{Volume: r.PathValue("namespace") + "/" + r.PathValue("volume"), Path: relative, Entries: []Entry{}}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			// An entry that disappeared between the read and the stat is not
			// part of this listing.
			continue
		}
		kind := "file"
		size := info.Size()
		if info.IsDir() {
			kind, size = "directory", 0
		}
		listing.Entries = append(listing.Entries, Entry{
			Name: entry.Name(), Kind: kind, Size: size, ModifiedAt: info.ModTime().UTC(),
		})
	}
	sort.SliceStable(listing.Entries, func(left, right int) bool {
		return listing.Entries[left].Name < listing.Entries[right].Name
	})
	writeJSON(w, http.StatusOK, listing)
}

// handleDownload streams one file.
func (s *Service) handleDownload(w http.ResponseWriter, r *http.Request) {
	session, failure := sessionOf(r)
	if failure != nil {
		controlplane.WriteError(w, failure)
		return
	}
	root, relative, failure := s.openVolume(r.Context(), session, r)
	if failure != nil {
		controlplane.WriteError(w, failure)
		return
	}
	defer root.Close()
	if relative == "" {
		controlplane.WriteError(w, invalidPath())
		return
	}
	file, err := root.Open(relative)
	if err != nil {
		controlplane.WriteError(w, s.readFailure(err))
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		controlplane.WriteError(w, invalidPath())
		return
	}
	name := path.Base(relative)
	if disposition := mime.FormatMediaType("attachment", map[string]string{"filename": name}); disposition != "" {
		w.Header().Set("Content-Disposition", disposition)
	}
	http.ServeContent(w, r, name, info.ModTime(), file)
}

// handleUpload replaces or creates one file.
func (s *Service) handleUpload(w http.ResponseWriter, r *http.Request) {
	session, failure := sessionOf(r)
	if failure != nil {
		controlplane.WriteError(w, failure)
		return
	}
	if failure := s.writeRole(session); failure != nil {
		controlplane.WriteError(w, failure)
		return
	}
	root, relative, failure := s.openVolume(r.Context(), session, r)
	if failure != nil {
		controlplane.WriteError(w, failure)
		return
	}
	defer root.Close()
	if relative == "" {
		controlplane.WriteError(w, invalidPath())
		return
	}
	content, err := io.ReadAll(io.LimitReader(r.Body, maxUploadBytes+1))
	if err != nil {
		controlplane.WriteError(w, s.writeFailure("读取上传内容失败", err))
		return
	}
	if len(content) > maxUploadBytes {
		controlplane.WriteError(w, invalidPathText(fmt.Sprintf("上传内容超过 %d 字节上限", maxUploadBytes)))
		return
	}
	if err := root.WriteFile(relative, content, 0o644); err != nil {
		controlplane.WriteError(w, s.writeFailure("写入文件失败", err))
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"volume": r.PathValue("namespace") + "/" + r.PathValue("volume"), "path": relative, "size": len(content),
	})
}

// renameRequest is the body of one rename.
type renameRequest struct {
	// From is the current path inside the volume.
	From string `json:"from"`
	// To is the new path inside the volume.
	To string `json:"to"`
}

// handleRename moves one entry inside the volume.
func (s *Service) handleRename(w http.ResponseWriter, r *http.Request) {
	session, failure := sessionOf(r)
	if failure != nil {
		controlplane.WriteError(w, failure)
		return
	}
	if failure := s.writeRole(session); failure != nil {
		controlplane.WriteError(w, failure)
		return
	}
	root, _, failure := s.openVolume(r.Context(), session, r)
	if failure != nil {
		controlplane.WriteError(w, failure)
		return
	}
	defer root.Close()
	var body renameRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		controlplane.WriteError(w, invalidPathText("重命名请求不是合法 JSON"))
		return
	}
	from, fromOK := cleanPath(body.From)
	to, toOK := cleanPath(body.To)
	if !fromOK || !toOK || from == "" || to == "" {
		controlplane.WriteError(w, invalidPath())
		return
	}
	if _, err := root.Stat(to); err == nil {
		controlplane.WriteError(w, conflictText("目标已经存在"))
		return
	}
	if err := root.Rename(from, to); err != nil {
		controlplane.WriteError(w, s.writeFailure("重命名失败", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"from": from, "to": to})
}

// handleDelete removes one file or empty directory.
func (s *Service) handleDelete(w http.ResponseWriter, r *http.Request) {
	session, failure := sessionOf(r)
	if failure != nil {
		controlplane.WriteError(w, failure)
		return
	}
	if failure := s.writeRole(session); failure != nil {
		controlplane.WriteError(w, failure)
		return
	}
	root, relative, failure := s.openVolume(r.Context(), session, r)
	if failure != nil {
		controlplane.WriteError(w, failure)
		return
	}
	defer root.Close()
	if relative == "" {
		controlplane.WriteError(w, invalidPath())
		return
	}
	if err := root.Remove(relative); err != nil {
		controlplane.WriteError(w, s.writeFailure("删除失败", err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// openVolume authorises one request against the exposed list, the volume object
// and the owner rule, and returns the volume's root handle with the path the
// request addresses. Every filesystem call goes through the handle, which
// refuses a path that leaves the volume, including through a symbolic link a
// co-writer of the volume planted (NG10, C-38).
func (s *Service) openVolume(ctx context.Context, session identity.Session, r *http.Request) (*os.Root, string, *controlplane.Error) {
	ref := VolumeRef{Namespace: r.PathValue("namespace"), Name: r.PathValue("volume")}
	if ref.Namespace == "" || ref.Name == "" {
		return nil, "", invalidPath()
	}
	refs, failure := s.exposedVolumes(ctx, session)
	if failure != nil {
		return nil, "", failure
	}
	if !contains(refs, ref) {
		return nil, "", notExposed(ref)
	}
	volume, err := s.volumes.Volume(ctx, ref.Namespace, ref.Name)
	if isNotFound(err) {
		return nil, "", missingVolume(ref)
	}
	if err != nil {
		return nil, "", s.failure("读取卷 "+ref.String()+" 失败", err)
	}
	if !ownerAllows(session, volume) {
		return nil, "", &controlplane.Error{
			Code:               controlplane.CodePermissionDenied,
			Message:            "只有卷的所有者可以访问卷内文件",
			Stage:              controlplane.StageAuthorize,
			RequiresUserAction: true,
			SideEffects:        controlplane.SideEffectsNone,
		}
	}
	directory, mounted := s.roots.Root(ref.Namespace, ref.Name)
	if !mounted {
		return nil, "", s.failure("卷 "+ref.String()+" 没有可用的数据面目录", nil)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, "", s.failure("卷 "+ref.String()+" 的数据面目录不可用", err)
	}
	relative, ok := cleanPath(r.URL.Query().Get("path"))
	if !ok {
		_ = root.Close()
		return nil, "", invalidPath()
	}
	return root, relative, nil
}

// exposedVolumes reads the account's exposed-volume setting.
func (s *Service) exposedVolumes(ctx context.Context, session identity.Session) ([]VolumeRef, *controlplane.Error) {
	if s.settings == nil {
		return nil, nil
	}
	values, err := s.settings.SettingValues(ctx, extension.SettingScopeUser, session.Username)
	if err != nil {
		return nil, s.failure("读取暴露的卷设置失败", err)
	}
	refs, err := ParseExposedVolumes(values[SettingExposedVolumes])
	if err != nil {
		return nil, invalidPathText(err.Error())
	}
	return refs, nil
}

// writeRole refuses a Guest's write (C-59).
func (s *Service) writeRole(session identity.Session) *controlplane.Error {
	if session.Role != write.RoleGuest {
		return nil
	}
	return &controlplane.Error{
		Code:               controlplane.CodePermissionDenied,
		Message:            "当前身份只读，不能修改卷内文件",
		Stage:              controlplane.StageAuthorize,
		RequiresUserAction: true,
		SideEffects:        controlplane.SideEffectsNone,
	}
}

// readFailure maps a filesystem read error onto the error envelope.
func (s *Service) readFailure(err error) *controlplane.Error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return &controlplane.Error{
			Code: controlplane.CodeNotFound, Message: "路径不存在", Stage: controlplane.StageValidate,
			RequiresUserAction: true, SideEffects: controlplane.SideEffectsNone,
		}
	case errors.Is(err, fs.ErrPermission):
		return &controlplane.Error{
			Code: controlplane.CodePermissionDenied, Message: "数据面无权读取该路径",
			Stage: controlplane.StageRead, RequiresUserAction: true, SideEffects: controlplane.SideEffectsNone,
		}
	}
	return s.failure("读取卷内文件失败", err)
}

// writeFailure maps a filesystem write error onto the error envelope.
func (s *Service) writeFailure(message string, err error) *controlplane.Error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return &controlplane.Error{
			Code: controlplane.CodeNotFound, Message: "路径或父目录不存在", Stage: controlplane.StageValidate,
			RequiresUserAction: true, SideEffects: controlplane.SideEffectsNone,
		}
	case errors.Is(err, fs.ErrPermission):
		return &controlplane.Error{
			Code: controlplane.CodePermissionDenied, Message: "数据面无权写入该路径",
			Stage: controlplane.StageApply, RequiresUserAction: true, SideEffects: controlplane.SideEffectsNone,
		}
	case errors.Is(err, syscall.ENOTEMPTY), errors.Is(err, syscall.EEXIST):
		return conflictText("目录非空或目标已经存在")
	}
	return s.failure(message, err)
}

// unavailable reports a data-plane failure the caller can retry.
func (s *Service) unavailable(w http.ResponseWriter, message string, err error) {
	controlplane.WriteError(w, s.failure(message, err))
}

// failure builds the envelope of a failure on this side of the service. The
// detail stays in the log: a system call error carries the data-plane host's
// directory layout, which the caller must not learn.
func (s *Service) failure(message string, err error) *controlplane.Error {
	if err != nil {
		s.log.Error(message, "error", err)
	}
	return &controlplane.Error{
		Code: controlplane.CodeWriteFailed, Message: message, Stage: controlplane.StageApply,
		Retryable: true, SideEffects: controlplane.SideEffectsNone,
	}
}

// invalidPath reports a path the data plane will not address.
func invalidPath() *controlplane.Error { return invalidPathText("路径无效") }

// invalidPathText reports one invalid input.
func invalidPathText(message string) *controlplane.Error {
	return &controlplane.Error{
		Code: controlplane.CodeInvalidField, Message: message, Stage: controlplane.StageValidate,
		RequiresUserAction: true, SideEffects: controlplane.SideEffectsNone,
	}
}

// conflictText reports a write whose precondition no longer holds.
func conflictText(message string) *controlplane.Error {
	return &controlplane.Error{
		Code: controlplane.CodeConflict, Message: message, Stage: controlplane.StageApply,
		RequiresUserAction: true, SideEffects: controlplane.SideEffectsNone,
	}
}

// notExposed reports a volume the account did not expose. The answer is a
// missing object, so the setting decides what exists for this caller.
func notExposed(ref VolumeRef) *controlplane.Error {
	return &controlplane.Error{
		Code: controlplane.CodeNotFound, Message: "卷 " + ref.String() + " 未在本账号暴露",
		Stage: controlplane.StageValidate, RequiresUserAction: true, SideEffects: controlplane.SideEffectsNone,
	}
}

// missingVolume reports a volume that no longer exists.
func missingVolume(ref VolumeRef) *controlplane.Error {
	return &controlplane.Error{
		Code: controlplane.CodeNotFound, Message: "卷 " + ref.String() + " 不存在",
		Stage: controlplane.StageValidate, RequiresUserAction: true, SideEffects: controlplane.SideEffectsNone,
	}
}

// sessionOf reads the session the account middleware put on the request.
func sessionOf(r *http.Request) (identity.Session, *controlplane.Error) {
	session, found := identity.FromContext(r.Context())
	if !found {
		return identity.Session{}, &controlplane.Error{
			Code: controlplane.CodeUnauthenticated, Message: "请求没有可用会话",
			Stage: controlplane.StageAuthorize, RequiresUserAction: true, SideEffects: controlplane.SideEffectsNone,
		}
	}
	return session, nil
}

// cleanPath turns a request path into a clean relative path. An absolute path
// or a parent step is refused instead of normalised: the name has to stay
// relative before the volume root is asked for it.
func cleanPath(raw string) (string, bool) {
	if raw == "" {
		return "", true
	}
	if strings.HasPrefix(raw, "/") || strings.ContainsRune(raw, 0) {
		return "", false
	}
	segments := make([]string, 0, 3)
	for _, segment := range strings.Split(raw, "/") {
		switch segment {
		case "", ".":
			continue
		case "..":
			return "", false
		default:
			segments = append(segments, segment)
		}
	}
	return strings.Join(segments, "/"), true
}

// insidePath turns a clean relative path into the name the volume root accepts:
// root methods address the root directory as ".".
func insidePath(relative string) string {
	if relative == "" {
		return "."
	}
	return relative
}

// writeJSON writes one JSON answer.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
