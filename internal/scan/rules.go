// Package scan is the ONE place Flare decides what counts as a secret or a
// piece of personal data in telemetry text.
//
// It exists because the same rule was written twice and extended once. The
// scrubber (internal/ai) and the ingest-time detector (internal/api) each kept
// their own copy of the prefixed-token regex; the scrubber gained six token
// families and dropped its minimum length, the detector did not. The result was
// a product that redacted a GitLab token on its way to a model while storing it
// in plain text, raising no flag, and never telling the org its credential had
// leaked.
//
// So there is one rule table here with two entry points:
//
//	Text/Version - rewrite the value (what leaves the tenant boundary)
//	Kinds        - name what was found (what the human is told)
//
// A rule that is added for one is available to the other by construction. Do
// not add a regex for either purpose anywhere else in the tree; guard_scrub_
// rules_test.go fails the build if a second copy appears.
package scan

import (
	"net"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// A rule is one recognisable shape, its replacement, and whether finding it is
// strong enough evidence to tell a human about it.
type rule struct {
	// kind is the label the detector reports ("jwt", "secret", "card",
	// "private-key", "personnummer"). Empty means scrub-only: the shape is
	// worth removing before egress but is too noisy to raise a badge and a
	// security event on. Emails, IP addresses, MAC addresses, hashes and bare
	// long numbers are all in that class deliberately.
	kind string

	re   *regexp.Regexp
	with string

	// keep is a second opinion on a candidate match. The regex finds the shape,
	// keep decides whether it is real (Luhn for cards, net.ParseIP for
	// compressed IPv6, a word-boundary parse for secret-ish key names). nil
	// means every match counts.
	keep func(m string) bool

	// bounded rejects a match whose immediate neighbours are word characters.
	// Go's regexp has no lookbehind, so \b cannot express "not glued to an
	// identifier": \b happily fires between ':' and 't' in "DB::table". That
	// one gap rewrote 62% of scope-resolution symbols in PHP, C++, Rust and
	// Ruby stack traces to "[ip]".
	bounded bool

	// notNextTo lists characters that disqualify a match when they sit
	// immediately before or after it, for shapes whose grammar is ambiguous
	// with something legitimate.
	notNextTo string

	// generic marks a shape that is about FORM rather than content: a hex run,
	// a long digit run, an address. Those rules are right for payload text and
	// wrong for an identifier field, where a 40-hex git SHA is the answer the
	// reader wanted, not a secret. Version() skips them.
	generic bool
}

// rePersonnummer matches a Swedish personnummer in either the 12-digit form or
// the separated 10-digit form. The month and day must be real; the check digit
// is verified separately (see the rule's keep). Named because the JSON walker
// needs the same shape test for a bare numeric leaf.
var rePersonnummer = regexp.MustCompile(`\b(?:(?:19|20)\d{2}(?:0[1-9]|1[0-2])(?:0[1-9]|[12]\d|3[01])[-+]?\d{4}|\d{2}(?:0[1-9]|1[0-2])(?:0[1-9]|[12]\d|3[01])[-+]\d{4})\b`)

// assignRule is named because Pair asks it the same question about a key that
// arrives without its value in the same string.
var assignRule = rule{
	// key=value / key: value / "key":"value" where the key NAMES a secret.
	// This is the only rule that catches a credential carrying no
	// recognisable prefix: a database password, an AWS secret access key, a
	// 32-hex Twilio token. Context is the evidence, so the key is parsed
	// rather than substring-matched (see keyNamesSecret): matching "token"
	// inside "tokens_used" turned an LLM cost counter into "[secret]", on a
	// product whose whole point is telling you what your models cost.
	kind: "secret",
	re:   regexp.MustCompile(`(?i)([A-Za-z0-9_.\-]*(?:passphrase|password|passwd|pwd|secret|token|credential|api[_-]?key|access[_-]?key|private[_-]?key|signing[_-]?key|encryption[_-]?key|session[_-]?key|client[_-]?secret|auth[_-]?token|refresh[_-]?token|apikey|connection[_-]?string|conn[_-]?str|database[_-]?url|db[_-]?url|webhook[_-]?url|dsn|ssn|personnummer|otp|pin)[A-Za-z0-9_.\-]*)(\s*["']?\s*[:=]\s*["']?)([^\s"',;)}<>]{4,})`),
	with: "${1}${2}[secret]",
	keep: func(m string) bool { return keyNamesSecret(assignKey(m)) },
}

// Structured, high-confidence secrets. These run first and use capture groups
// so the surrounding context survives the rewrite.
var structured = []rule{
	{
		// PEM private-key blocks of any type (RSA, EC, OPENSSH, plain).
		kind: "private-key",
		re:   regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
		with: "[private-key]",
	},
	{
		// PGP private keys use a different armour header, so the PEM rule above
		// never saw them.
		kind: "private-key",
		re:   regexp.MustCompile(`(?s)-----BEGIN PGP PRIVATE KEY BLOCK-----.*?-----END PGP PRIVATE KEY BLOCK-----`),
		with: "[private-key]",
	},
	{
		// A PEM header with no terminator. ai.Line truncates a field to 512
		// bytes and an RSA-2048 PEM is about 1700, so the rule above stopped
		// matching exactly when the key had already been cut in half and ~450
		// bytes of key material went to the model. Scrub order was fixed too
		// (scrub before flatten), but a header with no end marker is never
		// anything except a leaked key, whichever order runs.
		kind: "private-key",
		re:   regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----[\sA-Za-z0-9+/=]*`),
		with: "[private-key]",
	},
	{
		// Credentials embedded in a URL: scheme://user:PASSWORD@host.
		kind: "secret",
		re:   regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://[^:@/\s]+:)[^@/\s]+(@)`),
		with: "${1}[redacted]${2}",
	},
	{
		// Authorization: Bearer <token> / Basic <b64>, including the
		// percent-encoded separator that survives a URL-escaped log line.
		kind: "secret",
		re:   regexp.MustCompile(`(?i)\b(bearer|basic)(\s+|%20)[A-Za-z0-9._+/=\-]{8,}`),
		with: "${1}${2}[secret]",
	},
	{
		// The other Authorization schemes. "token" is only treated as a scheme
		// when the header name is actually there: on its own it is an ordinary
		// English word, and "token expiration" is not a credential.
		kind: "secret",
		re:   regexp.MustCompile(`(?i)\b(authorization\s*["']?\s*[:=]\s*["']?\s*(?:token|apikey|api-key)(?:\s+|%20))[A-Za-z0-9._+/=\-]{8,}`),
		with: "${1}[secret]",
	},
	assignRule,
}

// Payment cards sit between the structured pass and the generic pass: they must
// run before the bare-long-number rule, and they need a real issuer check
// rather than a regex (see card.go).
var cardRule = rule{
	kind: "card",
	re:   reCardCandidate,
	with: "[card]",
	keep: func(m string) bool { return IsPaymentCard(onlyDigits(m)) },
}

// Shape rules, applied in order after the structured pass.
var shapes = []rule{
	{
		kind: "jwt",
		re:   regexp.MustCompile(`eyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}`),
		with: "[jwt]",
	},
	{
		// Unsigned JWT (alg=none). Two segments, no signature. Carries the same
		// claims as a signed one and the three-segment rule never matched it.
		kind: "jwt",
		re:   regexp.MustCompile(`eyJ[A-Za-z0-9_-]{6,}\.eyJ[A-Za-z0-9_-]{6,}\.?`),
		with: "[jwt]",
	},
	{
		// Prefixed tokens. Stripe/OpenAI/Anthropic (sk/pk/rk), GitHub
		// (ghp/ghs/gho/ghu/ghr/github_pat), Slack (xox*/xapp), GitLab (glpat),
		// Shopify (shpat/shpca/shppa), DigitalOcean (dop_v1_), npm,
		// HuggingFace, Google OAuth client secret.
		//
		// The separator after the prefix is REQUIRED. Every one of these
		// families mints "<prefix><sep><random>", and without it the rule was
		// "any word starting sk/pk plus ten characters", which redacted
		// "skeletonLoader" out of a stack trace.
		kind: "secret",
		re:   regexp.MustCompile(`\b(?:sk|pk|rk|ghp|ghs|gho|ghu|ghr|github_pat|glpat|xox[baprs]|xapp|shpat|shpca|shppa|dop_v1|npm|hf|GOCSPX)[-_.][-_A-Za-z0-9]{10,}\b`),
		with: "[secret]",
	},
	{
		// AWS access-key ids are the exception: no separator, fixed alphabet.
		kind: "secret",
		re:   regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{12,20}\b`),
		with: "[secret]",
	},
	{
		// SendGrid: SG.<22>.<43>, two dot-separated segments after the prefix.
		kind: "secret",
		re:   regexp.MustCompile(`\bSG\.[A-Za-z0-9_\-]{16,}\.[A-Za-z0-9_\-]{16,}\b`),
		with: "[secret]",
	},
	{
		// Google API key. The exact-35 rule missed a key one character off, and
		// a key that does not match is a key that egresses.
		kind: "secret",
		re:   regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{30,45}\b`),
		with: "[secret]",
	},
	{
		// Swedish personnummer, both the 12-digit and the separated 10-digit
		// form. The date must be real and the check digit must pass, because
		// this one DOES raise a badge and a security event: a rule that fires on
		// one in ten ordinary ids is how the estate got paged about a payment
		// card that was a backup filename.
		kind: "personnummer",
		re:   rePersonnummer,
		with: "[personnummer]",
		keep: func(m string) bool {
			d := onlyDigits(m)
			if len(d) == 12 {
				d = d[2:]
			}
			return len(d) == 10 && luhn(d)
		},
	},
	{
		re:   regexp.MustCompile(`[\p{L}\p{N}._%+\-]+@[\p{L}\p{N}.\-]+\.[\p{L}]{2,}`),
		with: "[email]",
	},
	{
		// E.164 phone. Bounded so semver build metadata (1.0.0+20130313144700)
		// keeps its digits: the '+' there is glued to a version, not opening a
		// country code.
		re:      regexp.MustCompile(`\+[1-9]\d{7,14}`),
		with:    "[phone]",
		bounded: true,
		generic: true,
	},
	// IPv6 BEFORE IPv4: an IPv4-mapped address (::ffff:192.0.2.1) must be
	// matched whole, otherwise the IPv4 rule fires first and leaves ::ffff:[ip].
	{
		re:      regexp.MustCompile(`(?i)::(?:ffff:)?(?:\d{1,3}\.){3}\d{1,3}`),
		with:    "[ip]",
		generic: true,
	},
	{
		re:      regexp.MustCompile(`(?i)\b(?:[0-9a-f]{1,4}:){7}[0-9a-f]{1,4}\b`),
		with:    "[ip]",
		generic: true,
	},
	{
		// Compressed IPv6. The shape alone is not evidence: "DB::table" and
		// "std::vector" both match it, and hex-ish means [a-f], which is a large
		// share of real identifiers. So the candidate must survive TWO extra
		// checks, and it needs both. net.ParseIP alone accepts "e::ad" out of
		// the middle of "Cache::add"; the boundary check alone accepts
		// "DB::" standing on its own.
		re:      regexp.MustCompile(`(?i)(?:[0-9a-f]{1,4}:){1,7}:(?:[0-9a-f]{1,4}(?::[0-9a-f]{1,4}){0,6})?`),
		with:    "[ip]",
		bounded: true,
		keep:    func(m string) bool { return net.ParseIP(m) != nil },
		generic: true,
		// A colon on either side means the match is a slice out of a longer
		// scope chain, not an address. "a::b" parses as a valid IPv6 address on
		// its own, so ParseIP cannot separate it from the "a::b" inside
		// "a::b::c::d"; its neighbours can.
		notNextTo: ":",
	},
	{
		re:      regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`),
		with:    "[ip]",
		generic: true,
	},
	{
		re:      regexp.MustCompile(`\b(?:[0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}\b`),
		with:    "[mac]",
		generic: true,
	},
	{
		re:      regexp.MustCompile(`\b[A-Fa-f0-9]{40,}\b`),
		with:    "[hash]",
		generic: true,
	},
	{
		// Any remaining bare long digit run (unformatted, non-card).
		re:      regexp.MustCompile(`\b\d{13,19}\b`),
		with:    "[number]",
		generic: true,
	},
}

// Text scrubs free-form payload text: an exception message, a log body, a frame
// context line, a span attribute. Every rule applies.
func Text(s string) string { return apply(s, true) }

// Version scrubs an identifier-shaped field: a release, a version, an
// environment, a platform, a dist. Credential and personal-data rules apply;
// the generic form rules do not.
//
// A release in this estate is a git SHA, and the 40-hex rule rewrote every one
// of them to "[hash]". An agent asked "which deploy introduced this" then read
// "[hash]" from every first_release with nothing saying the field had been
// rewritten, while source-map symbolication kept working off the unscrubbed
// value, which made the loss look like a data problem rather than a scrubber
// problem. A git SHA is not a secret.
func Version(s string) string { return apply(s, false) }

// Field scrubs a value according to what the field is called, so a caller that
// walks a struct does not have to know the policy. Unknown names get Text,
// which is the conservative side.
func Field(name, s string) string {
	if IdentifierField(name) {
		return Version(s)
	}
	return Text(s)
}

// identifierFields are the response field names whose value is an identifier
// the reader needs intact. Declared once here so REST, MCP and the log shipper
// cannot disagree about which ones they are.
var identifierFields = map[string]bool{
	"release": true, "first_release": true, "last_release": true,
	"version": true, "environment": true, "env": true,
	"platform": true, "dist": true, "sdk": true,
	"trace_id": true, "span_id": true, "parent_span_id": true,
	"kind": true, "status": true, "severity": true, "level": true,
	"id": true, "project_id": true, "issue_id": true, "org_id": true,
	"slug": true, "fingerprint": true,
}

// IdentifierField reports whether a response field holds an identifier rather
// than payload prose.
func IdentifierField(name string) bool { return identifierFields[name] }

func apply(s string, generic bool) string {
	if s == "" {
		return s
	}
	s = normalize(s)
	for i := range structured {
		s, _ = structured[i].rewrite(s, true)
	}
	s, _ = cardRule.rewrite(s, true)
	for i := range shapes {
		if shapes[i].generic && !generic {
			continue
		}
		s, _ = shapes[i].rewrite(s, true)
	}
	return s
}

// Kinds returns the sorted, distinct labels for the sensitive shapes found in
// text, or nil when clean.
//
// This is the human-facing half of the same table Text uses. It reports only
// the high-confidence kinds: a flag that decorates every ordinary error carries
// no information, and it also raises a security event and seeds an alert.
func Kinds(text string) []string {
	if text == "" {
		return nil
	}
	text = normalize(text)
	found := map[string]bool{}
	check := func(r *rule) {
		if r.kind == "" || found[r.kind] {
			return
		}
		if _, hit := r.rewrite(text, false); hit {
			found[r.kind] = true
		}
	}
	for i := range structured {
		check(&structured[i])
	}
	check(&cardRule)
	for i := range shapes {
		check(&shapes[i])
	}
	if len(found) == 0 {
		return nil
	}
	kinds := make([]string, 0, len(found))
	for k := range found {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	return kinds
}

// rewrite scans s once. With replace it returns the rewritten string; without,
// it stops at the first surviving match and only reports that there was one.
func (r *rule) rewrite(s string, replace bool) (string, bool) {
	locs := r.re.FindAllStringSubmatchIndex(s, -1)
	if locs == nil {
		return s, false
	}
	var b strings.Builder
	last, hit := 0, false
	for _, loc := range locs {
		start, end := loc[0], loc[1]
		if r.bounded && !freeStanding(s, start, end) {
			continue
		}
		if r.notNextTo != "" && adjacentTo(s, start, end, r.notNextTo) {
			continue
		}
		if r.keep != nil && !r.keep(s[start:end]) {
			continue
		}
		if !replace {
			return s, true
		}
		hit = true
		b.WriteString(s[last:start])
		b.Write(r.re.ExpandString(nil, r.with, s, loc))
		last = end
	}
	if !hit {
		return s, false
	}
	b.WriteString(s[last:])
	return b.String(), true
}

// freeStanding reports whether s[start:end] is flanked by non-word characters,
// which is the "not glued to an identifier" test \b cannot express.
func freeStanding(s string, start, end int) bool {
	if start > 0 && isWordByte(s[start-1]) {
		return false
	}
	if end < len(s) && isWordByte(s[end]) {
		return false
	}
	return true
}

// adjacentTo reports whether s[start:end] is immediately preceded or followed
// by one of the given characters.
func adjacentTo(s string, start, end int, chars string) bool {
	if start > 0 && strings.IndexByte(chars, s[start-1]) >= 0 {
		return true
	}
	if end < len(s) && strings.IndexByte(chars, s[end]) >= 0 {
		return true
	}
	return false
}

func isWordByte(b byte) bool {
	return b == '_' || b >= 0x80 ||
		(b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// normalize strips the format characters that exist to hide text from a reader
// without changing what a consumer sees. A zero-width space inside a token is
// enough to walk a prefix rule, and neither a stack frame nor a log line ever
// needs one.
func normalize(s string) string {
	if !strings.ContainsFunc(s, isFormatRune) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if isFormatRune(r) {
			return -1
		}
		return r
	}, s)
}

func isFormatRune(r rune) bool {
	switch r {
	case '\u00ad', '\u200b', '\u200c', '\u200d', '\u2060', '\ufeff':
		return true
	}
	return unicode.Is(unicode.Cf, r)
}

// assignKey returns the key half of a key=value match.
func assignKey(m string) string {
	if i := strings.IndexAny(m, ":="); i >= 0 {
		return strings.TrimSpace(strings.Trim(m[:i], `"' `))
	}
	return m
}

// secretWords are whole identifier words that name a credential on their own.
var secretWords = map[string]bool{
	"passphrase": true, "password": true, "passwd": true, "pwd": true,
	"secret": true, "token": true, "credential": true, "credentials": true,
	"apikey": true, "authorization": true, "cookie": true, "cookies": true, "dsn": true,
	"ssn": true, "personnummer": true, "otp": true, "pin": true,
}

// secretPairs are two adjacent words that name a credential together. "key"
// alone is a map key; "api key" is not.
var secretPairs = map[string]bool{
	"api key": true, "access key": true, "private key": true, "signing key": true,
	"encryption key": true, "session key": true, "secret key": true,
	"client secret": true, "auth token": true, "refresh token": true,
	"access token": true, "id token": true, "bearer token": true,
	"connection string": true, "conn str": true, "database url": true,
	"db url": true, "webhook url": true,
}

// keyNamesSecret reports whether an identifier NAMES a credential, parsing it
// into words rather than searching it for substrings.
//
// The substring version redacted "tokens_used=1523400" and
// "tokenizer_name=gpt2-large", because both contain "token". LLM token
// accounting is exactly the telemetry a BYOAI product exists to show you, so
// the scrubber was destroying its own product's signal.
func keyNamesSecret(key string) bool {
	words := splitIdentifier(key)
	for i, w := range words {
		if secretWords[w] {
			return true
		}
		if i+1 < len(words) && secretPairs[w+" "+words[i+1]] {
			return true
		}
	}
	return false
}

// splitIdentifier breaks an identifier into lowercase words on separators and
// camelCase humps: "aws_secret_access_key" and "clientSecret" both split.
func splitIdentifier(s string) []string {
	var words []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			words = append(words, strings.ToLower(cur.String()))
			cur.Reset()
		}
	}
	runes := []rune(s)
	for i, r := range runes {
		switch {
		case r == '_' || r == '-' || r == '.' || r == ' ':
			flush()
		case unicode.IsUpper(r):
			// A hump starts a word, unless we are inside an all-caps run that is
			// not ending (so "APIKey" splits into "api" and "key").
			if cur.Len() > 0 && (unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1]) ||
				(i+1 < len(runes) && unicode.IsLower(runes[i+1]))) {
				flush()
			}
			cur.WriteRune(r)
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return words
}
