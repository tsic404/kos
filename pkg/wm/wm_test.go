package wm

import "testing"

func TestWindowSetKeepsInstancesApart(t *testing.T) {
	desktop := New(Size{Width: 800, Height: 600})
	first := desktop.Open("media")
	second := desktop.Open("media")
	if desktop.Count() != 2 || first.Instance != 1 || second.Instance != 2 {
		t.Fatalf("windows = %+v, want two instances", desktop.Windows())
	}
	if first.Width != 800 || first.Height != 600 || second.X != cascadeOffset {
		t.Fatalf("windows = %+v, want the default size and cascaded second window", desktop.Windows())
	}
	if !desktop.Minimize(first.ID) || !desktop.Windows()[0].IsMinimized || desktop.Count() != 2 {
		t.Fatalf("minimize = %+v, want a minimized window that is still open", desktop.Windows())
	}
	if !desktop.Close(first.ID) || desktop.Count() != 1 || desktop.Windows()[0].ID != second.ID {
		t.Fatalf("close = %+v, want the other instance left", desktop.Windows())
	}
	if desktop.Close(first.ID) || desktop.Minimize(first.ID) {
		t.Fatal("a closed window answered a later close or minimize")
	}
}

func TestCloseAllReportsTheWindowsItClosed(t *testing.T) {
	desktop := New(DefaultSize)
	desktop.Open("media")
	desktop.Open("notes")
	if closed := desktop.CloseAll(); closed != 2 || desktop.Count() != 0 {
		t.Fatalf("close all = %d (left %d), want 2 closed", closed, desktop.Count())
	}
	if closed := desktop.CloseAll(); closed != 0 {
		t.Fatalf("close all on an empty desktop = %d, want 0", closed)
	}
}

func TestLayoutRoundTrip(t *testing.T) {
	desktop := New(Size{Width: 640, Height: 480})
	desktop.Open("media")
	desktop.Open("media")
	desktop.Minimize(desktop.Windows()[0].ID)

	encoded, err := desktop.Layout().Encode()
	if err != nil {
		t.Fatalf("encode the layout: %v", err)
	}
	decoded, err := DecodeLayout(encoded)
	if err != nil {
		t.Fatalf("decode the layout: %v", err)
	}
	want := desktop.Layout()
	if len(decoded.Windows) != len(want.Windows) {
		t.Fatalf("decoded layout = %+v, want %+v", decoded, want)
	}
	for index := range want.Windows {
		if decoded.Windows[index] != want.Windows[index] {
			t.Fatalf("placement %d = %+v, want %+v", index, decoded.Windows[index], want.Windows[index])
		}
	}
	if _, err := DecodeLayout(""); err == nil {
		t.Fatal("an empty layout decoded")
	}
}

func TestSizeCodec(t *testing.T) {
	encoded, err := (Size{Width: 1024, Height: 640}).Encode()
	if err != nil {
		t.Fatalf("encode the size: %v", err)
	}
	if decoded, err := DecodeSize(encoded); err != nil || decoded != (Size{Width: 1024, Height: 640}) {
		t.Fatalf("decoded size = %+v (%v), want 1024x640", decoded, err)
	}
	if _, err := DecodeSize(`{"width":0,"height":0}`); err == nil {
		t.Fatal("a zero size decoded")
	}
}

func TestNewFallsBackToTheDefaultSize(t *testing.T) {
	desktop := New(Size{})
	window := desktop.Open("media")
	if desktop.DefaultSize() != DefaultSize || window.Width != DefaultSize.Width {
		t.Fatalf("default size = %+v, want %+v", desktop.DefaultSize(), DefaultSize)
	}
}
