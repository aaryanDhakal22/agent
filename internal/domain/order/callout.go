package order

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Finishing orders and the store hub's order callout (migration 00019).
// Contract: contract/agent.md §2.9.9-2.9.10.

// --- finishing -----------------------------------------------------------------

// FinishingSettings: when a ready pickup / counter order is done with. A
// terminal order finishes when it is picked up at a terminal; an online
// pickup finishes OnlinePickupMinutes after it was marked ready (0 = never,
// it waits for Close batch as before).
type FinishingSettings struct {
	OnlinePickupMinutes int `json:"online_pickup_minutes"`
}

// DefaultFinishing: online pickups finish 30 minutes after ready.
func DefaultFinishing() FinishingSettings { return FinishingSettings{OnlinePickupMinutes: 30} }

// Validate checks 0-240 minutes.
func (f FinishingSettings) Validate() error {
	if f.OnlinePickupMinutes < 0 || f.OnlinePickupMinutes > 240 {
		return fmt.Errorf("online_pickup_minutes must be 0-240")
	}
	return nil
}

// DecodeFinishing reads stored finishing settings over the defaults.
func DecodeFinishing(raw []byte) (FinishingSettings, error) {
	f := DefaultFinishing()
	if len(raw) == 0 {
		return f, nil
	}
	err := json.Unmarshal(raw, &f)
	return f, err
}

// --- the order callout -----------------------------------------------------------

// CalloutSettings drive the store hub's "Order ready for ticket number ..."
// on the store speaker (Quibase Settings > Order callout). The hub reads
// them from the feed's settings.changed (and /api/integration/config).
type CalloutSettings struct {
	Enabled bool `json:"enabled"`
	// Voice is a voice id from the hub's list (hub status callout.voices);
	// "" = the hub's default voice.
	Voice string `json:"voice"`
	// Volume of the voice, 0-100 %.
	Volume int `json:"volume"`
	// MusicDip: how far the music (Brave) is turned down while speaking,
	// 0-100 % (0 = not at all, 100 = silent).
	MusicDip int `json:"music_dip"`
	// RestoreSeconds: how long the music takes to come back up, 0-10 s.
	RestoreSeconds float64 `json:"restore_seconds"`
	// Chime: a short chime before the words.
	Chime bool `json:"chime"`
	// Repeat (2.3.4): how many times the phrase is said per callout, 1-3
	// (the chime once at the start, the music down for the whole sequence).
	Repeat int `json:"repeat"`
	// VoiceLines (2.3.8): a voice's own line, said instead of the normal
	// phrase whenever that voice speaks (chosen or guest). <x> (or {n}) is
	// the spoken ticket number; a line without it goes before the phrase.
	VoiceLines map[string]string `json:"voice_lines"`
	// GuestChance (2.3.8): percent of READY callouts spoken by a random
	// guest voice instead of Voice, 0-50 (0 = never).
	GuestChance int `json:"guest_chance"`
	// GuestVoices (2.3.8): the guest voice ids, comma separated ("" = every
	// online voice the hub has).
	GuestVoices string `json:"guest_voices"`
}

// MaxGuestChance / MaxGuestVoices bound the guest voices.
const (
	MaxGuestChance = 50
	MaxGuestVoices = 10
)

// Voice line bounds (CalloutSettings.VoiceLines).
const (
	MaxVoiceLines   = 60
	MaxVoiceLineLen = 160
)

// Callout repeat bounds (CalloutSettings.Repeat).
const (
	MinCalloutRepeat = 1
	MaxCalloutRepeat = 3
)

// DefaultCallout: on, the hub's default voice at 90 %, music down 70 %
// coming back over 1.5 s, chime first, said once, no voice lines.
func DefaultCallout() CalloutSettings {
	return CalloutSettings{Enabled: true, Volume: 90, MusicDip: 70, RestoreSeconds: 1.5, Chime: true, Repeat: 1, VoiceLines: map[string]string{}}
}

// MaxVoiceID bounds CalloutSettings.Voice.
const MaxVoiceID = 64

// Validate checks the ranges.
func (c CalloutSettings) Validate() error {
	switch {
	case c.Volume < 0 || c.Volume > 100:
		return fmt.Errorf("volume must be 0-100")
	case c.MusicDip < 0 || c.MusicDip > 100:
		return fmt.Errorf("music_dip must be 0-100")
	case c.RestoreSeconds < 0 || c.RestoreSeconds > 10:
		return fmt.Errorf("restore_seconds must be 0-10")
	case c.Repeat < MinCalloutRepeat || c.Repeat > MaxCalloutRepeat:
		return fmt.Errorf("repeat must be 1-3")
	case len(c.VoiceLines) > MaxVoiceLines:
		return fmt.Errorf("voice_lines: at most %d voices", MaxVoiceLines)
	case c.GuestChance < 0 || c.GuestChance > MaxGuestChance:
		return fmt.Errorf("guest_chance must be 0-%d", MaxGuestChance)
	}
	if err := ValidateGuestVoices(c.GuestVoices); err != nil {
		return err
	}
	for v, line := range c.VoiceLines {
		if err := ValidateVoice(v); err != nil || v == "" {
			return fmt.Errorf("voice_lines: %q is not a voice id", v)
		}
		if n := utf8.RuneCountInString(line); n == 0 || n > MaxVoiceLineLen || strings.ContainsAny(line, "\n\r") {
			return fmt.Errorf("voice_lines: a line is 1-%d characters on one line (leave a voice out for the normal phrase)", MaxVoiceLineLen)
		}
	}
	return ValidateVoice(c.Voice)
}

// ValidateGuestVoices checks the guest voice list: at most MaxGuestVoices
// voice ids, comma separated.
func ValidateGuestVoices(s string) error {
	n := 0
	for _, v := range strings.Split(s, ",") {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if n++; n > MaxGuestVoices {
			return fmt.Errorf("guest_voices: at most %d voices", MaxGuestVoices)
		}
		if err := ValidateVoice(v); err != nil {
			return fmt.Errorf("guest_voices: %w", err)
		}
	}
	return nil
}

// ValidateVoice checks a voice id ("" = the hub's default).
func ValidateVoice(voice string) error {
	if len(voice) > MaxVoiceID || strings.ContainsAny(voice, "/\\ \t\n") {
		return fmt.Errorf("voice must be a voice id from the hub's list")
	}
	return nil
}

// DecodeCallout reads stored callout settings over the defaults (settings
// stored before 2.3.4 have no repeat: said once).
func DecodeCallout(raw []byte) (CalloutSettings, error) {
	c := DefaultCallout()
	if len(raw) == 0 {
		return c, nil
	}
	err := json.Unmarshal(raw, &c)
	if c.VoiceLines == nil {
		c.VoiceLines = map[string]string{}
	}
	if c.Repeat < MinCalloutRepeat || c.Repeat > MaxCalloutRepeat {
		c.Repeat = DefaultCallout().Repeat
	}
	return c, err
}

// Callout test kinds (feed callout.test).
const (
	CalloutTest    = "test"    // TEST CALLOUT with a number
	CalloutPreview = "preview" // PREVIEW a voice
)

// CalloutTestData is a callout.test feed event: speak "Order ready for
// ticket number <number>" now with Voice ("" = the chosen voice), even with
// the callout switched off.
type CalloutTestData struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Number int    `json:"number"`
	Voice  string `json:"voice"`
}

// MaxCalloutNumber: ticket numbers the callout speaks (SoftTouch prints up
// to 4 digits).
const MaxCalloutNumber = 9999

// CalloutRenderData is a callout.render feed event (2.3.2): the tablet's
// PREVIEW "on this tablet" - the hub renders "Order ready for ticket number
// <number>" with Voice ("" = the chosen voice) and uploads the WAV to
// PUT /api/integration/callout-audio/<id>; nothing plays in the store.
type CalloutRenderData struct {
	ID     string `json:"id"`
	Voice  string `json:"voice"`
	Number int    `json:"number"`
}
