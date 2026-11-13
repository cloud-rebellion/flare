package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
	"unicode/utf8"

	"github.com/bright-interaction/flare/internal/scan"
)

const (
	maxEventTags        = 32
	maxEventTagKeyBytes = 64
	maxEventTagValBytes = 256

	// A read decodes up to 50 stored events, each up to the 8 MiB ingest body.
	// Stored JSONB orders keys by length, so "tags" sits near the start of any
	// ordinary event and this window only loses a document that pushed it out.
	maxEventTagScanBytes = 256 << 10
	// Top-level keys and tag entries looked at per document.
	maxEventTagScanEntries = 256
	// How much of one key or value the ingest detector reads, enough for a PEM.
	maxEventTagScanFieldBytes = 4 << 10
	// Total tag text the detector reads per event. Its regexes run inside the
	// ingest transaction, at about 1 MiB per second on the worst shape measured.
	maxEventTagScanTextBytes = 64 << 10
)

type eventTag = scan.KeyValue

// eventTags reads the tags the SDK sent from the stored payload, so events
// already received gain them too. Tags are client-written: only bounded string
// pairs are returned, in a stable order, and a malformed payload yields none.
// They are raw here, and the caller scrubs them with scan.Pairs.
func eventTags(payload json.RawMessage) []eventTag {
	decoded := decodeEventTags(payload[:min(len(payload), maxEventTagScanBytes)])
	tags := make([]eventTag, 0, min(len(decoded), maxEventTags))
	for _, tag := range decoded {
		if tag.Key != "" && len(tag.Key) <= maxEventTagKeyBytes && len(tag.Value) <= maxEventTagValBytes &&
			utf8.ValidString(tag.Key) && utf8.ValidString(tag.Value) {
			tags = append(tags, tag)
		}
	}
	sort.SliceStable(tags, func(i, j int) bool { return tags[i].Key < tags[j].Key })
	tags = tags[:min(len(tags), maxEventTags)]
	if len(tags) == 0 {
		return nil
	}
	return tags
}

// decodeEventTags streams the document so its work is bounded by the entry budget
// and the bytes handed to it, not by how many keys a sender chose to store. A
// later "tags" replaces an earlier one, as it does when the payload becomes JSONB.
func decodeEventTags(payload []byte) []eventTag {
	dec := json.NewDecoder(bytes.NewReader(payload))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil
	}
	var tags []eventTag
	budget := maxEventTagScanEntries
	for ; budget > 0 && dec.More(); budget-- {
		name, err := dec.Token()
		if err != nil {
			return tags
		}
		var raw json.RawMessage
		if dec.Decode(&raw) != nil {
			return tags
		}
		if name == "tags" {
			tags = decodeTagObject(raw, &budget)
		}
	}
	return tags
}

func decodeTagObject(raw []byte, budget *int) []eventTag {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil
	}
	var tags []eventTag
	for ; *budget > 0 && dec.More(); *budget-- {
		key, err := dec.Token()
		if err != nil {
			return tags
		}
		name, ok := key.(string)
		if !ok {
			return tags
		}
		var value *string
		if err := dec.Decode(&value); err != nil {
			var wrongType *json.UnmarshalTypeError
			if errors.As(err, &wrongType) {
				continue
			}
			return tags
		}
		if value != nil {
			tags = append(tags, eventTag{Key: name, Value: *value})
		}
	}
	return tags
}
