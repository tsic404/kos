package notification

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/state"
	"github.com/tsic404/kos/pkg/write"
)

// EventWindow is the window C-64 fixes for every event read: an event outside
// the hour that ends at the read is not delivered.
const EventWindow = controlplane.EventWindow

// eventsResource is the run kernel's event class C-64 reads.
var eventsResource = schema.GroupVersionResource{Version: "v1", Resource: "events"}

// Queue asks a controller set for one delivery round of a record (C-20).
type Queue interface {
	Enqueue(request reconcile.Request) error
}

// Service reads the recent events and turns every match into a delivery
// record. It is the read side of the product; the record's delivery is the
// declared controller's round (C-64, C-20).
type Service struct {
	// Dynamic reads the run kernel's events.
	Dynamic dynamic.Interface
	// Writer creates the delivery records as the extension controller (C-20).
	Writer *write.Writer
	// Settings reads the channel and subscription setting values (C-23).
	Settings Store
	// Queue asks for the delivery round of each new record; nil leaves the
	// records pending for a later pass.
	Queue Queue
	// Now supplies the read clock; defaults to time.Now.
	Now func() time.Time
}

// Report is what one read produced.
type Report struct {
	// Created names the records this pass created.
	Created []reconcile.Request
	// Skipped counts the matches whose record already existed.
	Skipped int
	// Problems lists the matches that could not be turned into a delivery, such
	// as a subscription naming a channel that is not configured.
	Problems []string
}

// Deliver reads the window's events and creates one record per matched
// subscription and channel. Repeating the read is idempotent: a record's name
// is derived from the event, the subscription and the channel.
func (s *Service) Deliver(ctx context.Context) (Report, error) {
	if s.Dynamic == nil || s.Writer == nil {
		return Report{}, fmt.Errorf("notification: the event reader or the record writer is not configured")
	}
	channelsRaw, subscriptionsRaw, err := settingValues(ctx, s.Settings)
	if err != nil {
		return Report{}, err
	}
	channels, err := ParseChannels(channelsRaw)
	if err != nil {
		return Report{}, err
	}
	subscriptions, err := ParseSubscriptions(subscriptionsRaw)
	if err != nil {
		return Report{}, err
	}
	known := make(map[string]Channel, len(channels))
	for _, channel := range channels {
		known[channel.ID] = channel
	}
	events, err := s.recentEvents(ctx)
	if err != nil {
		return Report{}, err
	}
	report := Report{}
	for _, event := range events {
		for _, subscription := range subscriptions {
			if !subscription.matches(event) {
				continue
			}
			for _, channelID := range subscription.Channels {
				channel, configured := known[channelID]
				if !configured {
					report.Problems = append(report.Problems,
						fmt.Sprintf("订阅 %s 引用未配置的渠道 %s", subscription.ID, channelID))
					continue
				}
				request := reconcile.Request{Kind: Kind, Name: recordName(event, subscription.ID, channel.ID)}
				_, err := s.Writer.Create(ctx, write.SubjectExtensionReconciler, write.Session{},
					recordObject(request.Name, channel.ID, subscription.ID, event))
				if apierrors.IsAlreadyExists(err) {
					report.Skipped++
					continue
				}
				if err != nil {
					return report, fmt.Errorf("create the delivery record of event %s: %w", event.Name, err)
				}
				report.Created = append(report.Created, request)
				if s.Queue == nil {
					continue
				}
				if err := s.Queue.Enqueue(request); err != nil {
					report.Problems = append(report.Problems,
						fmt.Sprintf("投递记录 %s 未进入投递队列：%v", request.Name, err))
				}
			}
		}
	}
	return report, nil
}

// recentEvents reads the events of the window, newest first (C-64). An event
// without a readable time cannot be judged against the window, so the read
// skips it instead of delivering something it cannot date.
func (s *Service) recentEvents(ctx context.Context) ([]controlplane.Event, error) {
	now := s.now()
	listed, err := s.Dynamic.Resource(eventsResource).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("read the recent events: %w", err)
	}
	cutoff := now.Add(-EventWindow)
	events := make([]controlplane.Event, 0, len(listed.Items))
	for _, item := range listed.Items {
		event, ok := decodeEvent(item.Object)
		if !ok || event.LastSeen.Before(cutoff) || event.LastSeen.After(now) {
			continue
		}
		events = append(events, event)
	}
	sort.SliceStable(events, func(left, right int) bool {
		if !events[left].LastSeen.Equal(events[right].LastSeen) {
			return events[left].LastSeen.After(events[right].LastSeen)
		}
		return events[left].Name < events[right].Name
	})
	return events, nil
}

// now reads the service clock.
func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// decodeEvent reads one stored event into the shape C-64 exposes.
func decodeEvent(object map[string]any) (controlplane.Event, bool) {
	name, _, _ := unstructured.NestedString(object, "metadata", "name")
	involved, found, _ := unstructured.NestedMap(object, "involvedObject")
	if name == "" || !found {
		return controlplane.Event{}, false
	}
	kind, _, _ := unstructured.NestedString(involved, "kind")
	subjectName, _, _ := unstructured.NestedString(involved, "name")
	if kind == "" || subjectName == "" {
		return controlplane.Event{}, false
	}
	lastSeen, ok := eventTime(object)
	if !ok {
		return controlplane.Event{}, false
	}
	namespace, _, _ := unstructured.NestedString(object, "metadata", "namespace")
	eventType, _, _ := unstructured.NestedString(object, "type")
	reason, _, _ := unstructured.NestedString(object, "reason")
	message, _, _ := unstructured.NestedString(object, "message")
	count, _, _ := unstructured.NestedInt64(object, "count")
	subjectNamespace, _, _ := unstructured.NestedString(involved, "namespace")
	apiVersion, _, _ := unstructured.NestedString(involved, "apiVersion")
	group := ""
	if before, _, found := strings.Cut(apiVersion, "/"); found {
		group = before
	}
	return controlplane.Event{
		Name:      name,
		Namespace: namespace,
		Type:      eventType,
		Reason:    reason,
		Message:   message,
		Count:     count,
		LastSeen:  lastSeen,
		InvolvedObject: controlplane.ObjectRef{
			Group: group, Kind: kind, Namespace: subjectNamespace, Name: subjectName,
		},
	}, true
}

// eventTime returns the newest timestamp one stored event carries. The core
// event class records it in lastTimestamp, and the newer eventTime field or the
// series carry it instead.
func eventTime(object map[string]any) (time.Time, bool) {
	var newest time.Time
	for _, path := range [][]string{
		{"lastTimestamp"}, {"eventTime"}, {"series", "lastObservedTime"}, {"firstTimestamp"},
	} {
		value, _, _ := unstructured.NestedString(object, path...)
		if value == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			continue
		}
		if parsed.After(newest) {
			newest = parsed
		}
	}
	return newest, !newest.IsZero()
}

// recordName derives a stable name from what one delivery is about, so a
// repeated read of the same event does not create a second record.
func recordName(event controlplane.Event, subscription, channel string) string {
	key := strings.Join([]string{
		event.InvolvedObject.Group, event.InvolvedObject.Kind,
		event.InvolvedObject.Namespace, event.InvolvedObject.Name,
		event.Namespace, event.Name, subscription, channel,
	}, "/")
	sum := sha256.Sum256([]byte(key))
	return "delivery-" + hex.EncodeToString(sum[:8])
}

// recordObject builds one delivery record without a status: the controller's
// first round is what writes the phase, so a stored record's empty status says
// nothing has been attempted yet.
func recordObject(name, channel, subscription string, event controlplane.Event) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": APIGroup + "/" + ObjectVersion,
		"kind":       Kind,
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"channel":      channel,
			"subscription": subscription,
			"event": map[string]any{
				"name":      event.Name,
				"namespace": event.Namespace,
				"type":      event.Type,
				"reason":    event.Reason,
				"message":   event.Message,
				"lastSeen":  event.LastSeen.UTC().Format(time.RFC3339Nano),
				"involvedObject": map[string]any{
					"group": event.InvolvedObject.Group, "kind": event.InvolvedObject.Kind,
					"namespace": event.InvolvedObject.Namespace, "name": event.InvolvedObject.Name,
				},
			},
		},
	}}
}

// Message is the payload a sender posts to an external channel.
type Message struct {
	// Subscription is the rule that matched the event.
	Subscription string `json:"subscription"`
	// Event is the event the record snapshotted.
	Event controlplane.Event `json:"event"`
}

// Sender posts one message to one channel. The secret is handed to the sender
// for this call only (C-14).
type Sender interface {
	Send(ctx context.Context, channel Channel, secret string, message Message) error
}

// Webhook posts the message as JSON. An empty secret sends no authorization
// header: the channel is unauthenticated, not misconfigured.
type Webhook struct {
	// Client sends the request; defaults to http.DefaultClient.
	Client *http.Client
}

// Send posts one message and rejects a non-2xx answer.
func (w Webhook) Send(ctx context.Context, channel Channel, secret string, message Message) error {
	body, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode the notification: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, channel.Endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build the notification request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if secret != "" {
		request.Header.Set("Authorization", "Bearer "+secret)
	}
	client := w.Client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("post to channel %s: %w", channel.ID, err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("channel %s answered %s", channel.ID, response.Status)
	}
	return nil
}

// Controller delivers the records of its class (C-20). Each round reads the
// record, resolves the channel from the settings, takes the channel's secret
// from the control plane and posts the message.
type Controller struct {
	// Writer reads the record and writes its status as the extension
	// controller.
	Writer *write.Writer
	// Settings reads the channel setting (C-23).
	Settings Store
	// Credentials is the control plane's own secret store (C-14).
	Credentials Credentials
	// Senders resolves a channel kind to its sender.
	Senders map[string]Sender
	// Log records a failed delivery without the credential (C-14); defaults to
	// discarding it.
	Log *slog.Logger
	// Now supplies the attempt clock; defaults to time.Now.
	Now func() time.Time
}

// Reconcile delivers one record. A delivered record is left alone, so a
// repeated round posts nothing twice; a refused delivery keeps the record and
// asks for a rate-limited retry (C-06, C-20).
func (c *Controller) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	if c.Writer == nil {
		return reconcile.Result{}, fmt.Errorf("notification: the record writer is not configured")
	}
	record, err := c.Writer.Get(ctx, write.Ref{Group: APIGroup, Version: ObjectVersion, Kind: Kind, Name: request.Name})
	if apierrors.IsNotFound(err) {
		return reconcile.Result{}, nil
	}
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("read delivery record %s: %w", request.Name, err)
	}
	phase, _, _ := unstructured.NestedString(record.Object, "status", "phase")
	if phase == PhaseDelivered {
		return reconcile.Result{}, nil
	}
	channelID, _, _ := unstructured.NestedString(record.Object, "spec", "channel")
	subscriptionID, _, _ := unstructured.NestedString(record.Object, "spec", "subscription")
	event, ok := recordEvent(record.Object)
	if !ok {
		return reconcile.Result{}, c.fail(ctx, record, "InvalidRecord", "投递记录没有可投递的事件")
	}
	channel, failure, err := c.channel(ctx, channelID)
	if err != nil {
		return reconcile.Result{}, err
	}
	if failure != nil {
		return reconcile.Result{}, c.fail(ctx, record, failure.reason, failure.message)
	}
	sender, known := c.Senders[channel.Kind]
	if !known {
		return reconcile.Result{}, c.fail(ctx, record, "ChannelKindUnsupported",
			fmt.Sprintf("渠道 %s 的类型 %s 没有发送实现", channel.ID, channel.Kind))
	}
	secret := ""
	if c.Credentials != nil {
		credential, registered, err := c.Credentials.ChannelCredential(ctx, channel.ID)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("read the credential of channel %s: %w", channel.ID, err)
		}
		if registered && credential.Endpoint != channel.Endpoint {
			// The endpoint is part of the user-writable setting, so a rewritten
			// endpoint must not receive the credential registered for this
			// channel; nothing is sent and the setting has to be corrected
			// (C-14).
			return reconcile.Result{}, c.fail(ctx, record, "ChannelEndpointMismatch",
				fmt.Sprintf("渠道 %s 的 endpoint 与登记凭据的地址不一致，未发送", channel.ID))
		}
		if registered {
			secret = credential.Secret
		}
	}
	if err := sender.Send(ctx, channel, secret, Message{Subscription: subscriptionID, Event: event}); err != nil {
		c.logger().Error("notification delivery failed", "record", record.GetName(), "channel", channel.ID, "error", err)
		// A channel that could not be reached may answer later, so the round is
		// retried within the loop's backoff (C-06).
		if writeErr := c.fail(ctx, record, "ChannelRejected", err.Error()); writeErr != nil {
			return reconcile.Result{}, writeErr
		}
		return reconcile.Result{}, err
	}
	if err := c.write(ctx, record, PhaseDelivered,
		[]metav1.Condition{state.Applied(record.GetGeneration(), "事件已投递到外部渠道")}); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

// channelFailure is a delivery the record cannot recover from by retrying:
// the channel setting has to be corrected first.
type channelFailure struct {
	reason  string
	message string
}

// channel resolves one configured channel. A store the control plane cannot
// read is returned as an error, so the round is retried instead of the record
// being failed; a setting the user has to correct is a terminal failure.
func (c *Controller) channel(ctx context.Context, id string) (Channel, *channelFailure, error) {
	channelsRaw, _, err := settingValues(ctx, c.Settings)
	if err != nil {
		return Channel{}, nil, err
	}
	channels, err := ParseChannels(channelsRaw)
	if err != nil {
		return Channel{}, &channelFailure{reason: "SettingsInvalid", message: err.Error()}, nil
	}
	for _, channel := range channels {
		if channel.ID == id {
			return channel, nil, nil
		}
	}
	return Channel{}, &channelFailure{reason: "ChannelUnknown",
		message: fmt.Sprintf("渠道 %s 不在通知设置中", id)}, nil
}

// recordEvent reads the event snapshot of one record.
func recordEvent(object map[string]any) (controlplane.Event, bool) {
	snapshot, found, _ := unstructured.NestedMap(object, "spec", "event")
	if !found {
		return controlplane.Event{}, false
	}
	name, _, _ := unstructured.NestedString(snapshot, "name")
	lastSeenText, _, _ := unstructured.NestedString(snapshot, "lastSeen")
	lastSeen, err := time.Parse(time.RFC3339Nano, lastSeenText)
	if name == "" || err != nil {
		return controlplane.Event{}, false
	}
	involved, _, _ := unstructured.NestedMap(snapshot, "involvedObject")
	typeName, _, _ := unstructured.NestedString(snapshot, "type")
	reason, _, _ := unstructured.NestedString(snapshot, "reason")
	message, _, _ := unstructured.NestedString(snapshot, "message")
	namespace, _, _ := unstructured.NestedString(snapshot, "namespace")
	group, _, _ := unstructured.NestedString(involved, "group")
	kind, _, _ := unstructured.NestedString(involved, "kind")
	subjectNamespace, _, _ := unstructured.NestedString(involved, "namespace")
	subjectName, _, _ := unstructured.NestedString(involved, "name")
	return controlplane.Event{
		Name: name, Namespace: namespace, Type: typeName, Reason: reason, Message: message, LastSeen: lastSeen,
		InvolvedObject: controlplane.ObjectRef{
			Group: group, Kind: kind, Namespace: subjectNamespace, Name: subjectName,
		},
	}, true
}

// fail records one delivery the record cannot recover from by retrying and
// counts the attempt. The reason stays machine-readable on its own condition;
// the message carries the human explanation.
func (c *Controller) fail(ctx context.Context, record *unstructured.Unstructured, reason, message string) error {
	generation := record.GetGeneration()
	return c.write(ctx, record, PhaseFailed, []metav1.Condition{
		state.Blocked(generation, ConditionDeliveryFailed, message),
		state.Failure(ConditionDeliveryFailed, generation, reason, message),
	})
}

// write stores the record's status and counts the attempt.
func (c *Controller) write(ctx context.Context, record *unstructured.Unstructured, phase string, conditions []metav1.Condition) error {
	attempts := attemptCount(record.Object) + 1
	now := c.now()
	status := recordStatus{
		Phase:         phase,
		Attempts:      attempts,
		LastAttemptAt: now.UTC().Format(time.RFC3339Nano),
		Conditions:    conditions,
	}
	if phase == PhaseDelivered {
		status.DeliveredAt = now.UTC().Format(time.RFC3339Nano)
	}
	converted, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&status)
	if err != nil {
		return fmt.Errorf("encode the delivery status: %w", err)
	}
	document := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": APIGroup + "/" + ObjectVersion,
		"kind":       Kind,
		"metadata":   map[string]any{"name": record.GetName()},
		"status":     converted,
	}}
	if _, err := c.Writer.Apply(ctx, write.SubjectExtensionReconciler, write.Session{}, write.PartObject, document); err != nil {
		c.logger().Error("notification status write failed", "record", record.GetName(), "error", err)
		return fmt.Errorf("write the status of delivery record %s: %w", record.GetName(), err)
	}
	return nil
}

// recordStatus is the status shape of one delivery record.
type recordStatus struct {
	Phase         string             `json:"phase,omitempty"`
	Attempts      int64              `json:"attempts"`
	LastAttemptAt string             `json:"lastAttemptAt,omitempty"`
	DeliveredAt   string             `json:"deliveredAt,omitempty"`
	Conditions    []metav1.Condition `json:"conditions,omitempty"`
}

// attemptCount reads how many deliveries the record already tried.
func attemptCount(object map[string]any) int64 {
	attempts, _, _ := unstructured.NestedInt64(object, "status", "attempts")
	return attempts
}

// now reads the controller clock.
func (c *Controller) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// logger returns the controller log.
func (c *Controller) logger() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.New(slog.DiscardHandler)
}
