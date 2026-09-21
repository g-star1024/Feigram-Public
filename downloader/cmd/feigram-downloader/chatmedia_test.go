package main

import (
	"testing"

	"github.com/gotd/td/tg"
)

// --- mediaLocationFromMessage ---------------------------------------------

func newTestDocument(id int64, mime string, size int64) *tg.Document {
	return &tg.Document{
		ID:            id,
		AccessHash:    987654321,
		FileReference: []byte("ref-bytes"),
		DCID:          2,
		MimeType:      mime,
		Size:          size,
		Attributes: []tg.DocumentAttributeClass{
			&tg.DocumentAttributeFilename{FileName: "clip.mp4"},
			&tg.DocumentAttributeVideo{Duration: 12, W: 640, H: 360},
		},
		Thumbs: []tg.PhotoSizeClass{
			&tg.PhotoSize{Type: "m", W: 320, H: 180, Size: 2048},
		},
	}
}

func documentTestMessage(doc *tg.Document) *tg.Message {
	media := &tg.MessageMediaDocument{}
	media.SetDocument(doc)
	message := &tg.Message{ID: 42}
	message.SetMedia(media)
	return message
}

func photoTestMessage(photo *tg.Photo) *tg.Message {
	media := &tg.MessageMediaPhoto{}
	media.SetPhoto(photo)
	message := &tg.Message{ID: 43}
	message.SetMedia(media)
	return message
}

func newTestPhoto() *tg.Photo {
	return &tg.Photo{
		ID:            555,
		AccessHash:    123456789,
		FileReference: []byte("photo-ref"),
		DCID:          2,
		Sizes: []tg.PhotoSizeClass{
			&tg.PhotoSize{Type: "s", W: 90, H: 90, Size: 900},
			&tg.PhotoSize{Type: "x", W: 1280, H: 960, Size: 128000},
			&tg.PhotoSize{Type: "m", W: 320, H: 240, Size: 32000},
		},
	}
}

func TestMediaLocationFromDocument(t *testing.T) {
	source, err := mediaLocationFromMessage(documentTestMessage(newTestDocument(11, "video/mp4", 4096)), false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	location, ok := source.Location.(*tg.InputDocumentFileLocation)
	if !ok {
		t.Fatalf("expected InputDocumentFileLocation, got %T", source.Location)
	}
	if location.ID != 11 || location.AccessHash != 987654321 {
		t.Fatalf("location mismatch: %+v", location)
	}
	if string(location.FileReference) != "ref-bytes" {
		t.Fatalf("file reference mismatch: %q", location.FileReference)
	}
	if location.ThumbSize != "" {
		t.Fatalf("full download must not set ThumbSize, got %q", location.ThumbSize)
	}
	if source.Size != 4096 || source.MimeType != "video/mp4" {
		t.Fatalf("meta mismatch: %+v", source)
	}
	if source.FileName != "clip.mp4" {
		t.Fatalf("expected filename from DocumentAttributeFilename, got %q", source.FileName)
	}
}

func TestMediaLocationFromDocumentUsesLastThumb(t *testing.T) {
	// Node 侧 mediaThumbnail 取 thumbs 的最后一项，Go 侧必须保持一致。
	source, err := mediaLocationFromMessage(documentTestMessage(newTestDocument(12, "video/mp4", 4096)), true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	location := source.Location.(*tg.InputDocumentFileLocation)
	if location.ThumbSize != "m" {
		t.Fatalf("expected thumb size m, got %q", location.ThumbSize)
	}
	if source.Size != 2048 {
		t.Fatalf("thumb size should report thumb bytes, got %d", source.Size)
	}
}

func TestMediaLocationFromDocumentWithoutThumb(t *testing.T) {
	doc := newTestDocument(13, "application/pdf", 1024)
	doc.Thumbs = nil
	if _, err := mediaLocationFromMessage(documentTestMessage(doc), true); err == nil {
		t.Fatal("expected error when document has no thumb")
	}
}

func TestMediaLocationFromPhotoPicksLargest(t *testing.T) {
	source, err := mediaLocationFromMessage(photoTestMessage(newTestPhoto()), false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	location, ok := source.Location.(*tg.InputPhotoFileLocation)
	if !ok {
		t.Fatalf("expected InputPhotoFileLocation, got %T", source.Location)
	}
	if location.ThumbSize != "x" {
		t.Fatalf("expected largest size x, got %q", location.ThumbSize)
	}
	if source.Size != 128000 {
		t.Fatalf("expected largest size bytes, got %d", source.Size)
	}
	if source.MimeType != "image/jpeg" {
		t.Fatalf("photo mime mismatch: %q", source.MimeType)
	}
}

func TestMediaLocationFromPhotoPicksSmallestForThumb(t *testing.T) {
	source, err := mediaLocationFromMessage(photoTestMessage(newTestPhoto()), true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	location := source.Location.(*tg.InputPhotoFileLocation)
	if location.ThumbSize != "s" {
		t.Fatalf("expected smallest size s, got %q", location.ThumbSize)
	}
}

func TestMediaLocationRejectsMediaLessMessage(t *testing.T) {
	if _, err := mediaLocationFromMessage(&tg.Message{ID: 1}, false); err == nil {
		t.Fatal("expected error for message without media")
	}
	if _, err := mediaLocationFromMessage(nil, false); err == nil {
		t.Fatal("expected error for nil message")
	}
}

func TestDocumentFileNameFallback(t *testing.T) {
	doc := newTestDocument(14, "application/zip", 10)
	doc.Attributes = nil
	if got := documentFileName(doc); got != "document-14" {
		t.Fatalf("expected fallback filename, got %q", got)
	}
}

// --- parseMediaRange -------------------------------------------------------

func TestParseMediaRange(t *testing.T) {
	cases := []struct {
		name      string
		header    string
		size      int64
		wantStart int64
		wantEnd   int64
		wantRange bool
	}{
		{"no header", "", 1000, 0, 0, false},
		{"invalid unit", "items=0-10", 1000, 0, 0, false},
		{"empty", "bytes=", 1000, 0, 0, false},
		{"open ended", "bytes=500-", 1000, 500, 999, true},
		{"closed", "bytes=100-199", 1000, 100, 199, true},
		{"suffix", "bytes=-200", 1000, 800, 999, true},
		{"end clamped", "bytes=100-9999", 1000, 100, 999, true},
		{"start beyond size", "bytes=2000-", 1000, 0, 0, false},
		{"suffix larger than size", "bytes=-5000", 1000, 0, 999, true},
		{"zero size", "bytes=0-", 0, 0, 0, true},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			start, end, isRange := parseMediaRange(item.header, item.size)
			if isRange != item.wantRange {
				t.Fatalf("isRange=%v want %v (start=%d end=%d)", isRange, item.wantRange, start, end)
			}
			if !item.wantRange {
				return
			}
			if start != item.wantStart || end != item.wantEnd {
				t.Fatalf("got [%d,%d] want [%d,%d]", start, end, item.wantStart, item.wantEnd)
			}
		})
	}
}

func TestParseMediaRangeReversedBounds(t *testing.T) {
	start, end, isRange := parseMediaRange("bytes=300-100", 1000)
	if !isRange || start != 300 || end != 300 {
		t.Fatalf("expected clamped single byte range, got [%d,%d] range=%v", start, end, isRange)
	}
}

func TestMediaBlobFileNameFallback(t *testing.T) {
	if got := mediaBlobFileName(nativeMediaSource{}); got != "telegram-media" {
		t.Fatalf("expected fallback name, got %q", got)
	}
	if got := mediaBlobFileName(nativeMediaSource{FileName: "a.mp4"}); got != "a.mp4" {
		t.Fatalf("expected given name, got %q", got)
	}
}
