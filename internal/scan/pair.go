package scan

import "strconv"

// KeyValue is one client-written key and value. A slice of them is scrubbed as
// pairs by Struct, because walking the two fields by name would lose the key.
type KeyValue struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Pair scrubs a key and value that arrive apart.
//
// The rule for a credential with no recognisable prefix needs key=value in one
// string, so `password` and `hunter2hunter2` scrubbed separately both looked
// harmless. The whole value goes when the key names a secret, because one-string
// matching stops at a space and a pair has no such boundary to rely on.
func Pair(key, value string) (string, string) {
	key = normalize(key)
	if value != "" && pairKeyNamesSecret(key) {
		return Text(key), "[secret]"
	}
	return Text(key), Field(key, value)
}

// pairKeyNamesSecret parses the whole key, then asks the one-string rule, which
// reads only the identifier nearest the separator: x/password is not an
// identifier, but x/password=hunter2hunter2 is scrubbed.
func pairKeyNamesSecret(key string) bool {
	if keyNamesSecret(key) {
		return true
	}
	_, hit := assignRule.rewrite(key+"=xxxx", false)
	return hit
}

// Pairs scrubs in place and returns pairs. Two distinct keys can scrub to one
// string, and a reader may treat keys as unique, so a collision gets the suffix
// scrubObjectAt uses. Safe to run twice.
func Pairs(pairs []KeyValue) []KeyValue {
	seen := make(map[string]bool, len(pairs))
	for i := range pairs {
		key, value := Pair(pairs[i].Key, pairs[i].Value)
		if seen[key] {
			for n := 2; ; n++ {
				cand := key + "#" + strconv.Itoa(n)
				if !seen[cand] {
					key = cand
					break
				}
			}
		}
		seen[key] = true
		pairs[i] = KeyValue{Key: key, Value: value}
	}
	return pairs
}
