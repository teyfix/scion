// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package config

import (
	"net/url"
	"regexp"
	"strings"
)

// redactedPlaceholder replaces a database connection string that could not
// be confidently parsed. Returning a fixed placeholder instead of the raw
// input guarantees that content we failed to recognize -- and therefore
// failed to vet for an embedded credential -- never reaches a log line.
const redactedPlaceholder = "<redacted>"

// redactedSecret replaces any password value RedactDatabaseURL masks. This
// matches the "xxxxx" convention already used by `scion admin promote`.
const redactedSecret = "xxxxx"

// connectionURLPattern matches the scheme prefix of a URL-style connection
// string, e.g. "postgres://" or "postgresql://".
var connectionURLPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://`)

// credentialQueryKeys lists query-string parameter names that carry a
// credential and must be masked wherever they appear, e.g. in the query
// string of a URL-style connection string.
var credentialQueryKeys = map[string]bool{
	"password":    true,
	"sslpassword": true,
}

// RedactDatabaseURL returns dsn with any embedded database credential
// masked, safe to write to a log line, error message, or stdout. It never
// returns the original password.
//
// It understands the connection string shapes used across this codebase:
//   - a URL-style connection string, e.g.
//     "postgres://user:pw@host/db?sslmode=require", including a
//     "host=/cloudsql/..." query parameter for Cloud SQL Unix sockets and a
//     URL-encoded password
//   - libpq keyword/value pairs, e.g.
//     `host=... user=... password=secret dbname=...`, including quoted
//     values
//   - a credential carried in a query parameter rather than (or in addition
//     to) the URL userinfo, e.g. "?password=..." or "&sslpassword=..."
//   - a bare SQLite path, which carries no credential and is returned
//     unchanged
//
// An empty dsn is always returned unchanged, as is any dsn when driver is
// "sqlite" (SQLite connection strings are filesystem paths, never
// credentials). Anything else that cannot be confidently parsed as one of
// the recognized shapes is replaced with a fixed placeholder rather than
// risk leaking a credential embedded in a format this function does not
// recognize.
func RedactDatabaseURL(driver, dsn string) string {
	if dsn == "" {
		return dsn
	}
	if strings.EqualFold(driver, "sqlite") {
		return dsn
	}
	switch {
	case connectionURLPattern.MatchString(dsn):
		return redactConnectionURL(dsn)
	case strings.Contains(dsn, "="):
		return redactKeywordValueDSN(dsn)
	default:
		// Not a recognizable URL or libpq keyword/value DSN. Since driver
		// isn't "sqlite" (handled above), we have no basis for trusting this
		// is a harmless bare path -- fail safe rather than risk an
		// unrecognized credential format reaching a log line.
		return redactedPlaceholder
	}
}

// redactKeywordValueDSN scans libpq keyword/value tokens before masking
// credentials. Backslash escapes consume the following byte even when it is
// whitespace. Malformed tokens fail closed rather than expose a partial value.
func redactKeywordValueDSN(dsn string) string {
	var out strings.Builder
	last := 0
	for i := 0; i < len(dsn); {
		for i < len(dsn) && isDSNSpace(dsn[i]) {
			i++
		}
		if i == len(dsn) {
			break
		}
		start := i
		for i < len(dsn) && !isDSNSpace(dsn[i]) && dsn[i] != '=' {
			i++
		}
		key := dsn[start:i]
		for i < len(dsn) && isDSNSpace(dsn[i]) {
			i++
		}
		if !isDSNKeyword(key) || i == len(dsn) || dsn[i] != '=' {
			return redactedPlaceholder
		}
		i++
		for i < len(dsn) && isDSNSpace(dsn[i]) {
			i++
		}
		var quote byte
		if i < len(dsn) && (dsn[i] == '\'' || dsn[i] == '"') {
			quote = dsn[i]
			i++
		}
		closed := quote == 0
		for i < len(dsn) {
			if dsn[i] == '\\' {
				if i+1 == len(dsn) {
					return redactedPlaceholder
				}
				i += 2
				continue
			}
			if quote != 0 && dsn[i] == quote {
				i++
				closed = true
				break
			}
			if quote == 0 && isDSNSpace(dsn[i]) {
				break
			}
			i++
		}
		if !closed || (i < len(dsn) && !isDSNSpace(dsn[i])) {
			return redactedPlaceholder
		}
		if credentialQueryKeys[strings.ToLower(key)] {
			out.WriteString(dsn[last:start])
			out.WriteString(key)
			out.WriteByte('=')
			out.WriteString(redactedSecret)
			last = i
		}
	}
	out.WriteString(dsn[last:])
	return out.String()
}

func isDSNKeyword(key string) bool {
	if key == "" {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}

func isDSNSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

// redactConnectionURL masks the password in a URL-style connection string,
// whether it appears in the userinfo component or as a query parameter. It
// falls back to a placeholder if dsn cannot be parsed as a URL at all.
func redactConnectionURL(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return redactedPlaceholder
	}
	if u.User != nil {
		if _, has := u.User.Password(); has {
			u.User = url.UserPassword(u.User.Username(), redactedSecret)
		}
	}
	if u.RawQuery != "" {
		u.RawQuery = redactQueryCredentials(u.RawQuery)
	}
	return u.String()
}

// redactQueryCredentials masks any credential parameter (see
// credentialQueryKeys) in a raw URL query string, leaving every other
// parameter -- including an unencoded "host=/cloudsql/..." value -- exactly
// as it was.
func redactQueryCredentials(rawQuery string) string {
	pairs := strings.Split(rawQuery, "&")
	for i, pair := range pairs {
		if pair == "" {
			continue
		}
		key := pair
		if idx := strings.IndexByte(pair, '='); idx >= 0 {
			key = pair[:idx]
		}
		unescapedKey, err := url.QueryUnescape(key)
		if err != nil {
			unescapedKey = key
		}
		if credentialQueryKeys[strings.ToLower(unescapedKey)] {
			pairs[i] = key + "=" + redactedSecret
		}
	}
	return strings.Join(pairs, "&")
}
