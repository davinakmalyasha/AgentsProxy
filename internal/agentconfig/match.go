package agentconfig

import (
	"strings"
	"unicode"
)

// HasPhrase reports whether kw occurs in words as a whole word, or a
// contiguous run of whole words.
//
// Deliberately a copy of the router's matcher rather than a shared import.
// The router lives in internal/agent and this package is loaded from
// projects/ before any tool exists; making agentconfig depend on agent to
// borrow one function would invert the dependency and pull the whole tool
// registry into config parsing.
//
// The two are pinned to agree by TestMatcherAgreesWithTheRouter, which runs
// the router's own table against this implementation. If they ever drift,
// one of them is matching substrings again -- which is how "pesanan" reached
// book_slot and every data question came back empty.
func HasPhrase(words []string, kw string) bool {
	if kw == "" {
		return false
	}
	k := Tokenize(Normalise(kw))
	if len(k) == 0 || len(k) > len(words) {
		return false
	}
outer:
	for i := 0; i+len(k) <= len(words); i++ {
		for j := range k {
			if words[i+j] != k[j] {
				continue outer
			}
		}
		return true
	}
	return false
}

// Normalise lowercases and collapses punctuation to single spaces, so
// "Jam Tersibuk?" and "jam-tersibuk" are the same message.
func Normalise(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := true
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			prevSpace = false
			continue
		}
		if !prevSpace {
			b.WriteByte(' ')
			prevSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

// Tokenize splits a normalised string into words.
func Tokenize(s string) []string {
	return strings.Fields(Normalise(s))
}

// Words normalises and tokenises a raw user message.
func Words(message string) []string {
	if message == "" {
		return nil
	}
	return strings.Fields(Normalise(message))
}
