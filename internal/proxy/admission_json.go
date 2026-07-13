package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// canonicalRequestRoute ensures policy matching, persisted attribution, and the
// forwarded URL all describe the same path. Encoded slashes/dot segments and
// non-canonical RawPath values are rejected instead of being interpreted
// differently by Burnban, net/http, and an upstream reverse proxy.
func canonicalRequestRoute(u *url.URL) (string, error) {
	if u == nil || u.Opaque != "" || !strings.HasPrefix(u.Path, "/") {
		return "", fmt.Errorf("request URL must use an absolute hierarchical path")
	}
	if !utf8.ValidString(u.Path) || len(u.Path) > maxPolicyAdmissionLabelBytes {
		return "", fmt.Errorf("request path must be valid UTF-8 of at most %d bytes", maxPolicyAdmissionLabelBytes)
	}
	for _, r := range u.Path {
		if r == '\\' || unicode.IsControl(r) {
			return "", fmt.Errorf("request path contains an ambiguous character")
		}
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return "", fmt.Errorf("request path must not contain dot segments")
		}
	}
	canonical := (&url.URL{Path: u.Path}).EscapedPath()
	if u.RawPath != "" && u.RawPath != canonical {
		return "", fmt.Errorf("request path uses a non-canonical percent encoding")
	}
	return canonical, nil
}

// transportErrorDetail deliberately omits url.Error.URL, which can include a
// custom upstream's secret path or query string. Network-operation errors are
// useful to operators and contain only operation/address/cause metadata.
func transportErrorDetail(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Op + ": " + transportErrorDetail(urlErr.Err)
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		return netErr.Error()
	}
	return fmt.Sprintf("%T", err)
}

// validateAdmissionJSON rejects duplicate keys before Burnban extracts model,
// output bounds, service tier, geo, and provider-tool metadata. Different JSON
// parsers disagree on first-key versus last-key wins; forwarding an ambiguous
// object would let policy and the provider evaluate different requests.
func validateAdmissionJSON(body []byte, contentType string) error {
	trimmed := bytes.TrimSpace(body)
	isJSON := false
	if mediaType, _, err := mime.ParseMediaType(contentType); err == nil {
		isJSON = mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
	} else if strings.Contains(strings.ToLower(contentType), "json") {
		isJSON = true
	}
	if len(trimmed) != 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
		isJSON = true
	}
	if !isJSON {
		return nil
	}
	if len(trimmed) == 0 {
		return fmt.Errorf("JSON request body is empty")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	if err := scanAdmissionJSON(dec, 0); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("JSON request contains multiple top-level values")
		}
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return nil
}

func scanAdmissionJSON(dec *json.Decoder, depth int) error {
	if depth > 256 {
		return fmt.Errorf("JSON request nesting exceeds 256 levels")
	}
	token, err := dec.Token()
	if err != nil {
		return fmt.Errorf("invalid JSON request: %w", err)
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		seenFolded := map[string]string{}
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return fmt.Errorf("invalid JSON request: %w", err)
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("JSON object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("JSON request contains duplicate field %q", key)
			}
			folded := foldAdmissionKey(key)
			if previous, exists := seenFolded[folded]; exists && previous != key {
				return fmt.Errorf("JSON request contains case-ambiguous fields %q and %q", previous, key)
			}
			if canonical, securityKey := canonicalAdmissionKeys[folded]; securityKey && canonical != key {
				return fmt.Errorf("JSON request uses non-canonical security field %q; use %q", key, canonical)
			}
			seen[key] = struct{}{}
			seenFolded[folded] = key
			if err := scanAdmissionJSON(dec, depth+1); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return fmt.Errorf("invalid JSON request: %w", err)
		}
	case '[':
		for dec.More() {
			if err := scanAdmissionJSON(dec, depth+1); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return fmt.Errorf("invalid JSON request: %w", err)
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
	return nil
}

func foldAdmissionKey(value string) string {
	var out strings.Builder
	out.Grow(len(value))
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out.WriteByte(c)
	}
	return out.String()
}

var canonicalAdmissionKeys = func() map[string]string {
	out := map[string]string{}
	for _, key := range []string{
		"model", "max_tokens", "max_output_tokens", "max_completion_tokens", "service_tier",
		"inference_geo", "generationConfig", "maxOutputTokens", "tools", "type",
		"googleSearch", "googleSearchRetrieval", "googleMaps", "urlContext", "codeExecution", "retrieval",
	} {
		out[foldAdmissionKey(key)] = key
	}
	return out
}()
