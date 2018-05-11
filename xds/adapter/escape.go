/*
Copyright 2018 Turbine Labs, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package adapter

// Cookie, Header and Query Parameter escaping strategy. Whereas:
// 1. Envoy has no direct support for cookies. Cookies can only be
//    generated by appending "Set-Cookie" headers.
// 2. Envoy performs no escaping of generated headers. This includes
//    static headers defined directly in Envoy's config and generated
//    by interpolation of % expressions (e.g., UPSTREAM_METADATA). It
//    is trivial to generate a header containing illegal header
//    values.
// 3. Legal cookie values are much more restrictive than legal header values.
// 4. Legal query parameter values are even more restrictive.
//
// Therefore, our strategy for metadata values is: escape metadata
// values to be legal cookie and query parameter values in all cases.
//
// When matching header values, generate regular expressions that
// match both the fully escaped (legal for query parameters) value or
// a partially-escaped (legal for headers) value. When matching cookie
// values, generate regular expressions that match for the
// fully-escaped version or a partially-escaped (legal for cookies)
// value. When matching query parameters, match only the fully-escaped
// version. Further, when a header value matcher does not require
// regex matching (e.g. because it contains no escaped characters),
// use a non-regex match for performance. Cookie value matchers are
// always regexes because of how the cookies are encoded in headers.
//
// Examples (represented as go strings):
// Value          Escaped Value   Header Match       Cookie/Query Match
// "simple"       "simple"        "simple"           "simple"
// "hdr;safe"     "hdr%3Bsafe"    "hdr(%3B|;)safe"   "hdr%3Bsafe"
// "un\tsafe"     "un%09safe"     "un%09safe"        "un%09safe"
// "b=o\th"       "b%3Do%09h"     "b(%3D|=)o%09h"    "b%3Do%09h"

// flags indicates whether a particular byte value is safe
// (e.g. allowed) in headers, cookies, or regular expressions.
type flags uint8

const (
	headerSafe flags = 1 << iota
	cookieSafe
	querySafe
	regexSafe

	safe flags = headerSafe | cookieSafe | querySafe | regexSafe
)

func (f flags) isHeaderSafe() bool { return f&headerSafe != 0 }
func (f flags) isCookieSafe() bool { return f&cookieSafe != 0 }
func (f flags) isQuerySafe() bool  { return f&querySafe != 0 }
func (f flags) isRegexSafe() bool  { return f&regexSafe != 0 }

var (
	// byteFlags is initialized to a 256-element array containing one
	// flag for each possible byte value.
	byteFlags []flags

	hex = []byte{'0', '1', '2', '3', '4', '5', '6', '7', '8', '9', 'a', 'b', 'c', 'd', 'e', 'f'}
)

func init() {
	// Initialize all byteFlags to "safe"
	byteFlags = make([]flags, 0x100)
	for b := 0; b < 0x100; b++ {
		byteFlags[b] = safe
	}

	// All control characters and 8-bit ascii chars are not safe for
	// headers (and therefore not for cookies).
	for b := 0; b < 0x20; b++ {
		byteFlags[b] &= ^(headerSafe | cookieSafe)
	}

	for b := 0x7f; b < 0x100; b++ {
		byteFlags[b] &= ^(headerSafe | cookieSafe)
	}

	// Additional characters that are not safe for cookies.
	for _, b := range []byte{' ', '"', '%', ',', ';', '\\', '~'} {
		byteFlags[b] &= ^cookieSafe
	}

	// Unreserved characters per https://tools.ietf.org/html/rfc3986
	// are alphanumerics, hyphen, period, underscore and tilde.
	for b := 0; b < 0x100; b++ {
		switch {
		case b == '-' || b == '_' || b == '.' || b == '~':
			// ok
		case b >= 'A' && b <= 'Z':
			// ok
		case b >= 'a' && b <= 'z':
			// ok
		case b >= '0' && b <= '9':
			// ok
		default:
			byteFlags[b] &= ^querySafe
		}
	}

	// Regex characters that require escaping to be treated as
	// literals.
	for _, b := range []byte{
		'\\', '.', '+', '*', '?', '(', ')', '|', '[', ']', '{', '}', '^', '$',
	} {
		byteFlags[b] &= ^regexSafe
	}
}

// regexMode is used to indicate what level of regex escaping is
// required for a text transformation. NoEscape implies regular
// expressions are not in use, therefore no regular expression
// patterns should be emitted and no regex special characters need
// escaping. AlwaysEscape means the result is always assumed to be a
// regular expression, so regex special character must always be
// escaped. DynamicEscape means that regex special characters must be
// escaped, but only if a regex pattern is emitted elsewhere in the
// transformation.
type regexMode int

const (
	noEscape regexMode = iota
	alwaysEscape
	dynamicEscape
)

type encodingType int

const (
	notEncoded encodingType = iota
	percentEncoded
	regexEncoded
)

// transformer takes a text input and performs a transformation based
// the configured len and replacement functions.
type transformer struct {
	// Returns size of encoded byte and how it will be encoded.
	len func(b byte, mode regexMode) (int, encodingType)

	// Returns the encoded byte (which may just be the original
	// byte). The escapeRegex flag indicates that regex special
	// characters must be escaped.
	replacement func(b byte, escapeRegex bool) []byte

	// Default mode of the transformer.
	mode regexMode
}

var (
	metadataEscaper = &transformer{metadataEscapeLen, metadataEscape, noEscape}
	headerMatcher   = &transformer{headerMatcherLen, headerMatcherEscape, dynamicEscape}
	cookieMatcher   = &transformer{cookieMatcherLen, cookieMatcherEscape, alwaysEscape}
	queryMatcher    = &transformer{queryMatcherLen, queryMatcherEscape, noEscape}
)

// Transforms the string and returns true if the output is a regular
// expression.
func (t *transformer) transform(s string) (string, bool) {
	// Copy default mode.
	mode := t.mode

	bytes := []byte(s)

	// Compute the output size.
	resultBytes := 0
	changed := false
	for i := 0; i < len(bytes); {
		n, encoding := t.len(bytes[i], mode)
		if mode == dynamicEscape && encoding == regexEncoded {
			// We've emitted a regex expression and must restart to
			// insure that any previously un-escaped regex special
			// characters are counted correctly.
			mode = alwaysEscape
			i = 0
			resultBytes = 0
			continue
		}

		resultBytes += n
		if encoding != notEncoded {
			changed = true
		}

		i++
	}

	// If nothing changed, we report the output as a regex only if the
	// original mode is to always escape. Otherwise mode is noEscape
	// or mode is dynamicEscape and no regex pattern was required.
	if !changed {
		return s, t.mode == alwaysEscape
	}

	// If mode is still dynamicEscape, no regex pattern was required.
	isRegex := mode == alwaysEscape

	result := make([]byte, resultBytes)

	// resultPos always spans from the current insertion point to the
	// end of result.
	resultPos := result
	for _, b := range bytes {
		replacement := t.replacement(b, isRegex)
		n := copy(resultPos, replacement)
		resultPos = resultPos[n:]
	}

	return string(result), isRegex
}

func metadataEscapeLen(b byte, mode regexMode) (int, encodingType) {
	if mode != noEscape {
		panic("metadata escaping does not support regex escaping")
	}

	if !byteFlags[b].isCookieSafe() {
		return 3, percentEncoded
	}
	return 1, notEncoded
}

func metadataEscape(b byte, escapeRegex bool) []byte {
	if escapeRegex {
		panic("metadata escaping does not support regex escaping")
	}

	if !byteFlags[b].isCookieSafe() {
		return []byte{'%', hex[b>>4], hex[b&0xF]}
	}
	return []byte{b}
}

func cookieMatcherLen(b byte, mode regexMode) (int, encodingType) {
	if mode != alwaysEscape {
		panic("cookie value escaping always performs regex escaping")
	}

	f := byteFlags[b]

	if !f.isCookieSafe() {
		return 3, percentEncoded
	}

	if !f.isQuerySafe() {
		if !f.isRegexSafe() {
			return 8, regexEncoded
		}

		return 7, regexEncoded
	}

	if !f.isRegexSafe() {
		return 2, regexEncoded
	}

	return 1, notEncoded
}

func cookieMatcherEscape(b byte, escapeRegex bool) []byte {
	if !escapeRegex {
		panic("cookie value escaping always performs regex escaping")
	}

	f := byteFlags[b]

	if !f.isCookieSafe() {
		return []byte{'%', hex[b>>4], hex[b&0xF]}
	}

	if !f.isQuerySafe() {
		if !f.isRegexSafe() {
			return []byte{'(', '%', hex[b>>4], hex[b&0xF], '|', '\\', b, ')'}
		}

		return []byte{'(', '%', hex[b>>4], hex[b&0xF], '|', b, ')'}
	}

	if !f.isRegexSafe() {
		return []byte{'\\', b}
	}

	return []byte{b}

}

func headerMatcherLen(b byte, mode regexMode) (int, encodingType) {
	if mode == noEscape {
		panic("header matchers may require regex escapes")
	}

	f := byteFlags[b]

	if !f.isHeaderSafe() {
		return 3, percentEncoded
	}

	if !f.isCookieSafe() || !f.isQuerySafe() {
		if !f.isRegexSafe() {
			return 8, regexEncoded
		}

		return 7, regexEncoded
	}

	if !f.isRegexSafe() && mode == alwaysEscape {
		return 2, regexEncoded
	}

	return 1, notEncoded
}

func headerMatcherEscape(b byte, escapeRegex bool) []byte {
	f := byteFlags[b]

	if !f.isHeaderSafe() {
		return []byte{'%', hex[b>>4], hex[b&0xF]}
	}

	if !f.isCookieSafe() || !f.isQuerySafe() {
		if !escapeRegex {
			panic("header matcher regex output disabled, but required")
		}

		if !f.isRegexSafe() {
			return []byte{'(', '%', hex[b>>4], hex[b&0xF], '|', '\\', b, ')'}
		}

		return []byte{'(', '%', hex[b>>4], hex[b&0xF], '|', b, ')'}
	}

	// Only escape these characters if we're emitting a regular
	// expression (for other characters). E.g. if the value has a
	// period in it but no other %-encoded characters, we do not need
	// to escape the period.
	if !f.isRegexSafe() && escapeRegex {
		return []byte{'\\', b}
	}

	return []byte{b}
}

func queryMatcherLen(b byte, mode regexMode) (int, encodingType) {
	if mode != noEscape {
		panic("query matchers are never regexes")
	}

	f := byteFlags[b]

	if !f.isQuerySafe() || !f.isCookieSafe() {
		return 3, percentEncoded
	}

	return 1, notEncoded
}

func queryMatcherEscape(b byte, escapeRegex bool) []byte {
	if escapeRegex {
		panic("query matchers are never regexes")
	}

	f := byteFlags[b]

	if !f.isQuerySafe() || !f.isCookieSafe() {
		return []byte{'%', hex[b>>4], hex[b&0xF]}
	}

	return []byte{b}
}

// Escape the given string to be safe as a cookie value (which implies
// safety as a header value). See
// https://tools.ietf.org/html/rfc6265#section-4.1). Additionally,
// escapes '%' because we use it as an escape character for hex codes.
func escapeMetadata(value string) string {
	escaped, _ := metadataEscaper.transform(value)
	return escaped
}

// Produces a string suitable for matching escaped metadata in header
// values in an Envoy header matcher. If the boolean return value is
// true, the string is a regular expression.
func headerMatcherForMetadata(value string) (string, bool) {
	return headerMatcher.transform(value)
}

// Produces a regular expression string suitable for matching escaped
// metadata in cookie values in an Envoy header matcher. Caller is
// responsible for matching the cookie name. Any regular expression
// characters in the metadata are escaped with backslashes.
func cookieMatcherForMetadata(value string) string {
	escaped, _ := cookieMatcher.transform(value)
	return escaped
}

// Produces a string literal suitable for matching escaped metadata in
// query parameter values in an Envoy query parameter matcher.
func queryMatcherForMetadata(value string) string {
	escaped, _ := queryMatcher.transform(value)
	return escaped
}