// Copyright 2024-2026 Remi Philippe
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package connector

import (
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"maunium.net/go/mautrix/event"
)

func TestParseMMVoice(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		props     model.StringInterface
		wantNil   bool
		wantDurMS int
		wantWave  []int
	}{
		{"no props", nil, true, 0, nil},
		{"no voice key", model.StringInterface{"other": 1}, true, 0, nil},
		{"voice wrong type", model.StringInterface{"voice": "x"}, true, 0, nil},
		{"duration and waveform", model.StringInterface{"voice": map[string]any{
			"duration": 1.9, "waveform": []any{10.0, 100.0, 50.0}}}, false, 1900, []int{102, 1024, 512}},
		{"clamps and skips junk", model.StringInterface{"voice": map[string]any{
			"duration": -3.0, "waveform": []any{500.0, "x", -5.0}}}, false, 0, []int{1024, 0}},
		{"empty voice", model.StringInterface{"voice": map[string]any{}}, false, 0, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := parseMMVoice(&model.Post{Props: tt.props})
			if tt.wantNil {
				if got != nil {
					t.Fatalf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("expected voice, got nil")
			}
			if got.DurationMS != tt.wantDurMS {
				t.Errorf("DurationMS: got %d, want %d", got.DurationMS, tt.wantDurMS)
			}
			if len(got.Waveform) != len(tt.wantWave) {
				t.Fatalf("Waveform: got %v, want %v", got.Waveform, tt.wantWave)
			}
			for i := range got.Waveform {
				if got.Waveform[i] != tt.wantWave[i] {
					t.Errorf("Waveform[%d]: got %d, want %d", i, got.Waveform[i], tt.wantWave[i])
				}
			}
		})
	}
}

func TestDownsampleWaveform(t *testing.T) {
	t.Parallel()
	for _, in := range [][]int{
		{0}, {1024}, {0, 0, 0}, {5, 900, 20}, make([]int, 1000), {-5, -1},
	} {
		out := downsampleWaveform(in)
		if len(out) != mmWaveformSamples {
			t.Fatalf("len: got %d, want %d", len(out), mmWaveformSamples)
		}
		for i, v := range out {
			if v < mmWaveformMin || v > mmWaveformMax {
				t.Errorf("input %v: out[%d]=%d outside %d..%d", in, i, v, mmWaveformMin, mmWaveformMax)
			}
		}
	}
	// The peak sample maps to the maximum.
	out := downsampleWaveform([]int{0, 0, 361, 0})
	max := 0
	for _, v := range out {
		if v > max {
			max = v
		}
	}
	if max != mmWaveformMax {
		t.Errorf("peak: got %d, want %d", max, mmWaveformMax)
	}
}

func TestBuildMMVoiceProps(t *testing.T) {
	t.Parallel()
	c := &event.MessageEventContent{
		MSC1767Audio: &event.MSC1767Audio{Duration: 2148, Waveform: []int{0, 100, 1024}},
	}
	props, secs := buildMMVoiceProps(c)
	if secs != 2.148 {
		t.Errorf("secs: got %v", secs)
	}
	if props["duration"] != 2.1 {
		t.Errorf("duration: got %v, want 2.1", props["duration"])
	}
	if wf, ok := props["waveform"].([]int); !ok || len(wf) != mmWaveformSamples {
		t.Errorf("waveform: got %v", props["waveform"])
	}

	// Falls back to Info.Duration; no waveform key without samples.
	c2 := &event.MessageEventContent{Info: &event.FileInfo{Duration: 5000}}
	props, secs = buildMMVoiceProps(c2)
	if secs != 5 || props["duration"] != 5.0 {
		t.Errorf("fallback: secs=%v props=%v", secs, props)
	}
	if _, has := props["waveform"]; has {
		t.Error("unexpected waveform key")
	}

	// Nothing known at all.
	if _, secs := buildMMVoiceProps(&event.MessageEventContent{}); secs != 0 {
		t.Errorf("empty: secs=%v", secs)
	}
}

func TestVoiceLabelAndFileName(t *testing.T) {
	t.Parallel()
	if got := voiceLabel(7.4); got != "🎤 Voice message (0:07)" {
		t.Errorf("got %q", got)
	}
	if got := voiceLabel(125); got != "🎤 Voice message (2:05)" {
		t.Errorf("got %q", got)
	}
	c := &event.MessageEventContent{Info: &event.FileInfo{MimeType: "audio/ogg"}}
	ts := time.Date(2026, 11, 16, 12, 22, 16, 0, time.UTC)
	if got := voiceFileName("Voice message", c, ts); got != "voice-message-20261116-122216.ogg" {
		t.Errorf("got %q", got)
	}
	if got := voiceFileName("rec.m4a", c, ts); got != "voice-message-20261116-122216.m4a" {
		t.Errorf("got %q", got)
	}
}

func TestIsVoicePlaceholderText(t *testing.T) {
	t.Parallel()
	if !isVoicePlaceholderText("🎤 Voice message (0:01)") || !isVoicePlaceholderText("  🎤 x") {
		t.Error("placeholder not recognised")
	}
	if isVoicePlaceholderText("hello") || isVoicePlaceholderText("") {
		t.Error("plain text treated as placeholder")
	}
}

func TestConvertPostToMatrix_NativeVoicePost(t *testing.T) {
	t.Parallel()
	fake := newFakeMM()
	t.Cleanup(fake.Close)
	fake.Files["v1"] = &model.FileInfo{Id: "v1", Name: "voice-message-20261116-142216.m4a", MimeType: "audio/mp4", Size: 29020}

	mc := newFullTestClient(fake.Server.URL)
	post := &model.Post{
		Id: "p1", ChannelId: "ch1", UserId: "u1",
		Type:    "custom_voice",
		Message: "🎤 Voice message (0:02)",
		FileIds: model.StringArray{"v1"},
		Props: model.StringInterface{"voice": map[string]any{
			"duration": 1.9, "waveform": []any{10.0, 67.0, 100.0}}},
	}
	msg := mc.convertPostToMatrix(post)

	if len(msg.Parts) != 1 {
		t.Fatalf("expected only the audio part (placeholder text dropped), got %d", len(msg.Parts))
	}
	c := msg.Parts[0].Content
	if c.MsgType != event.MsgAudio || c.MSC3245Voice == nil {
		t.Fatalf("expected voice audio, got %+v", c)
	}
	if c.MSC1767Audio == nil || c.MSC1767Audio.Duration != 1900 || len(c.MSC1767Audio.Waveform) != 3 {
		t.Errorf("MSC1767Audio: %+v", c.MSC1767Audio)
	}
	if c.Info.Duration != 1900 {
		t.Errorf("Info.Duration: got %d", c.Info.Duration)
	}
}

func TestConvertPostToMatrix_VoiceCaptionKept(t *testing.T) {
	t.Parallel()
	fake := newFakeMM()
	t.Cleanup(fake.Close)
	fake.Files["v1"] = &model.FileInfo{Id: "v1", Name: "a.m4a", MimeType: "audio/mp4"}

	mc := newFullTestClient(fake.Server.URL)
	post := &model.Post{
		Id: "p1", Message: "listen to this", FileIds: model.StringArray{"v1"},
		Props: model.StringInterface{"voice": map[string]any{"duration": 1.0}},
	}
	msg := mc.convertPostToMatrix(post)
	if len(msg.Parts) != 2 || msg.Parts[0].Content.Body != "listen to this" {
		t.Fatalf("a real caption must be kept, got %d parts", len(msg.Parts))
	}
}

func FuzzParseMMVoice(f *testing.F) {
	f.Add(1.9, 50.0, 10.0)
	f.Add(-1.0, 1e308, -1e308)
	f.Fuzz(func(t *testing.T, dur, a, b float64) {
		v := parseMMVoice(&model.Post{Props: model.StringInterface{"voice": map[string]any{
			"duration": dur, "waveform": []any{a, b},
		}}})
		if v == nil {
			t.Fatal("voice map must parse")
		}
		if v.DurationMS < 0 {
			t.Fatalf("negative duration %d", v.DurationMS)
		}
		for _, w := range v.Waveform {
			if w < 0 || w > matrixWaveformMax {
				t.Fatalf("waveform value %d out of range", w)
			}
		}
		for _, w := range downsampleWaveform(v.Waveform) {
			if w < mmWaveformMin || w > mmWaveformMax {
				t.Fatalf("downsampled value %d out of range", w)
			}
		}
	})
}
