package playback

import (
	"strings"

	"github.com/Silo-Server/silo-server/internal/lang"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/userstore"
)

// OriginalLanguageSentinel is the value stored in audio language preference
// columns to mean "use the media item's original language." It is resolved
// to a concrete language code in the playback handler before reaching
// SelectAudioTrack.
const OriginalLanguageSentinel = "original"

// AudioTrackPreference holds a per-series audio track preference.
type AudioTrackPreference struct {
	AudioTrackIndex int
	AudioLanguage   string
	TrackSignature  *userstore.AudioTrackSignature
}

func langMatch(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return lang.Canonical(a) == lang.Canonical(b)
}

// trackHasLanguage reports whether the track carries the preferred language,
// either as its primary code or anywhere in its MULTi language list.
func trackHasLanguage(track models.AudioTrack, preferred string) bool {
	if langMatch(track.Language, preferred) {
		return true
	}
	if preferred == "" || len(track.Languages) == 0 {
		return false
	}
	canonical := lang.Canonical(preferred)
	for _, code := range track.Languages {
		if lang.Canonical(code) == canonical {
			return true
		}
	}
	return false
}

// SelectAudioTrack determines which audio track to use based on preferences.
//
// Priority:
// 1. Series preference exact track signature
// 2. Series preference index (if track exists at that index with matching language)
// 3. Series preference language (first track matching that language)
// 4. Profile preferred language (first track matching)
// 5. File's default track (first track with Default: true)
// 6. First track (index 0)
func SelectAudioTrack(tracks []models.AudioTrack, preferredLang string, seriesPref *AudioTrackPreference) int {
	if len(tracks) == 0 {
		return 0
	}

	// 1. Series preference: try exact signature match first.
	if seriesPref != nil {
		if idx := findExactAudioTrack(tracks, seriesPref.TrackSignature); idx >= 0 {
			return idx
		}

		// 2. Series preference: try exact index+language match.
		if seriesPref.AudioTrackIndex >= 0 && seriesPref.AudioTrackIndex < len(tracks) {
			if trackHasLanguage(tracks[seriesPref.AudioTrackIndex], seriesPref.AudioLanguage) {
				return seriesPref.AudioTrackIndex
			}
		}

		// 3. Series preference: fall back to language match.
		if seriesPref.AudioLanguage != "" {
			for i, t := range tracks {
				if trackHasLanguage(t, seriesPref.AudioLanguage) {
					return i
				}
			}
		}
	}

	// 4. Profile language preference.
	if preferredLang != "" {
		for i, t := range tracks {
			if trackHasLanguage(t, preferredLang) {
				return i
			}
		}
	}

	// 5. File's default track.
	for i, t := range tracks {
		if t.Default {
			return i
		}
	}

	// 6. First track.
	return 0
}

// MatchAudioTrackAcrossVersions maps a selection made against one file's
// audio inventory onto another version of the same content. Track ordering is
// not stable across encodes, so carrying the raw ordinal can select a different
// language. Prefer the stable signature, then the selected language, and
// finally the effective file's default track.
func MatchAudioTrackAcrossVersions(
	requestedTracks []models.AudioTrack,
	effectiveTracks []models.AudioTrack,
	requestedIndex int,
) int {
	if len(effectiveTracks) == 0 {
		return 0
	}
	if len(requestedTracks) == 0 {
		return SelectAudioTrack(effectiveTracks, "", nil)
	}
	if requestedIndex < 0 || requestedIndex >= len(requestedTracks) {
		requestedIndex = SelectAudioTrack(requestedTracks, "", nil)
	}

	selected := requestedTracks[requestedIndex]
	signature := AudioTrackSignatureFromTrack(selected)
	// A signature match is language-independent and authoritative: the same
	// track on another encode keeps its identity even if the language list is
	// ordered differently.
	if idx := findExactAudioTrack(effectiveTracks, signature); idx >= 0 {
		return idx
	}
	// A MULTi/undetermined track's primary Language is "mul"/"und" and matches
	// nothing; it carries several concrete languages instead. Try every
	// language the requested track carries, in order, before falling back to
	// the effective file's default. Reducing to a single code (the old
	// Languages[0] behavior) degraded to default whenever that one language was
	// absent even though a later one was present.
	for _, code := range crossVersionAudioLanguages(selected) {
		candidate := SelectAudioTrack(effectiveTracks, "", &AudioTrackPreference{
			AudioTrackIndex: requestedIndex,
			AudioLanguage:   code,
			TrackSignature:  signature,
		})
		if trackHasLanguage(effectiveTracks[candidate], code) {
			return candidate
		}
	}
	// No carried language resolved on the target; keep the previous
	// default/first-track fallback.
	return SelectAudioTrack(effectiveTracks, "", nil)
}

// crossVersionAudioLanguages returns the concrete languages a track carries,
// primary code first, then its MULTi language list, deduplicated by canonical
// form. The "und"/"mul" sentinels are placeholders and are skipped.
func crossVersionAudioLanguages(track models.AudioTrack) []string {
	codes := make([]string, 0, len(track.Languages)+1)
	primary := track.Language
	if primary != "" && primary != "und" && primary != "mul" {
		codes = append(codes, primary)
	}
	for _, code := range track.Languages {
		if code == "" {
			continue
		}
		duplicate := false
		for _, existing := range codes {
			if langMatch(existing, code) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			codes = append(codes, code)
		}
	}
	return codes
}

// BrowserSupportsAudioCodec returns true if the given audio codec can be
// played natively by web browsers without transcoding.
func BrowserSupportsAudioCodec(codec string) bool {
	switch strings.ToLower(codec) {
	case "aac", "mp3", "opus", "vorbis", "flac":
		return true
	default:
		return false
	}
}
