// Copyright 2024-2026 Remi Philippe
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package connector

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"maunium.net/go/mautrix/event"
)

// Some Mattermost deployments implement voice messages with a plugin: a post
// of a custom type carrying one audio file, a "🎤 Voice message (m:ss)" text
// and props.voice = {duration: <seconds>, waveform: [<10..100>, ...]}.
// These helpers translate between that shape and Matrix voice messages
// (MSC3245 / MSC1767: duration in ms, waveform values 0..1024).

const (
	voicePlaceholderPrefix = "🎤"
	mmWaveformSamples      = 48
	mmWaveformMin          = 10
	mmWaveformMax          = 100
	matrixWaveformMax      = 1024
)

// mmVoice is the parsed props.voice of a Mattermost post.
type mmVoice struct {
	DurationMS int
	Waveform   []int // Matrix scale (0..1024); may be empty
}

// parseMMVoice extracts props.voice from a post, or nil if it has none.
func parseMMVoice(post *model.Post) *mmVoice {
	raw, ok := post.Props["voice"].(map[string]any)
	if !ok {
		return nil
	}
	v := &mmVoice{}
	if d, ok := raw["duration"].(float64); ok && d > 0 && !math.IsInf(d, 0) && !math.IsNaN(d) {
		v.DurationMS = int(math.Round(d * 1000))
	}
	if wf, ok := raw["waveform"].([]any); ok {
		for _, x := range wf {
			f, ok := x.(float64)
			if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
				continue
			}
			f = math.Max(0, math.Min(mmWaveformMax, f))
			v.Waveform = append(v.Waveform, int(math.Round(f*matrixWaveformMax/mmWaveformMax)))
		}
	}
	return v
}

// isVoicePlaceholderText reports whether a post message is just the plugin's
// "🎤 Voice message (0:01)" label, which is redundant next to a Matrix voice event.
func isVoicePlaceholderText(msg string) bool {
	return strings.HasPrefix(strings.TrimSpace(msg), voicePlaceholderPrefix)
}

// applyMMVoice turns an audio message content into a Matrix voice message.
func applyMMVoice(content *event.MessageEventContent, v *mmVoice) {
	content.MSC3245Voice = &event.MSC3245Voice{}
	if content.Info == nil {
		content.Info = &event.FileInfo{}
	}
	content.Info.Duration = v.DurationMS
	content.MSC1767Audio = &event.MSC1767Audio{Duration: v.DurationMS, Waveform: v.Waveform}
}

// buildMMVoiceProps builds the props.voice map for a Matrix voice message and
// returns it with the duration in seconds.
func buildMMVoiceProps(content *event.MessageEventContent) (map[string]any, float64) {
	durMS := 0
	var wave []int
	if content.MSC1767Audio != nil {
		durMS = content.MSC1767Audio.Duration
		wave = content.MSC1767Audio.Waveform
	}
	if durMS <= 0 && content.Info != nil {
		durMS = content.Info.Duration
	}
	secs := float64(durMS) / 1000

	props := map[string]any{"duration": math.Round(secs*10) / 10}
	if len(wave) > 0 {
		props["waveform"] = downsampleWaveform(wave)
	}
	return props, secs
}

// downsampleWaveform resamples a Matrix waveform to mmWaveformSamples values in
// the 10..100 range Mattermost's player expects, normalised to the peak.
func downsampleWaveform(wave []int) []int {
	peak := 1
	for _, w := range wave {
		if w > peak {
			peak = w
		}
	}
	out := make([]int, mmWaveformSamples)
	for i := range out {
		start := i * len(wave) / mmWaveformSamples
		end := (i + 1) * len(wave) / mmWaveformSamples
		if end <= start {
			end = start + 1
		}
		if end > len(wave) {
			end = len(wave)
		}
		sum := 0
		for _, w := range wave[start:end] {
			if w < 0 {
				w = 0
			}
			sum += w
		}
		avg := float64(sum) / float64(end-start)
		out[i] = mmWaveformMin + int(math.Round(avg*(mmWaveformMax-mmWaveformMin)/float64(peak)))
	}
	return out
}

// voiceLabel formats the placeholder text, e.g. "🎤 Voice message (0:07)".
func voiceLabel(secs float64) string {
	total := int(math.Round(secs))
	return fmt.Sprintf("%s Voice message (%d:%02d)", voicePlaceholderPrefix, total/60, total%60)
}

// voiceFileName names an uploaded voice message like the plugin does, keeping
// the extension of the original name (or derived from its MIME type).
func voiceFileName(orig string, content *event.MessageEventContent, now time.Time) string {
	name := fileNameWithExt(orig, content)
	ext := ""
	if i := strings.LastIndex(name, "."); i >= 0 {
		ext = name[i:]
	}
	return "voice-message-" + now.UTC().Format("20060102-150405") + ext
}
