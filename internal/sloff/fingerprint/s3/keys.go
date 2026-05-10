package s3

import (
	"fmt"
	"strings"

	"github.com/izumin5210/sloff/internal/sloff/fingerprint"
)

// timestampWidth mirrors the local backend's filename prefix width
// (YYYYMMDDHHMMSSsss = 14 calendar digits + 3 millisecond digits, ADR-0010).
// Keeping the constant here avoids importing the local package just for it
// and lets keys.go stay free of cross-backend coupling.
const timestampWidth = 17

// objectPrefix returns the directory-shaped key prefix for a (spec, task)
// pair: "<rootPrefix>/<spec_relpath>/<task_id>/". A trailing slash is always
// emitted so callers can pass the result straight to ListObjectsV2 as a
// Prefix without ambiguity between "spec_a" and "spec_a_extra".
//
// rootPrefix may be empty (the caller uses the bucket root) or a forward-slash
// path; we strip surrounding slashes so callers can pass either form.
func objectPrefix(rootPrefix string, key fingerprint.Key) string {
	var b strings.Builder
	if rp := strings.Trim(rootPrefix, "/"); rp != "" {
		b.WriteString(rp)
		b.WriteByte('/')
	}
	if key.SpecRelpath != "" {
		b.WriteString(strings.Trim(key.SpecRelpath, "/"))
		b.WriteByte('/')
	}
	b.WriteString(key.TaskID)
	b.WriteByte('/')
	return b.String()
}

// rootPrefix is objectPrefix without spec/task scoping; useful for the full
// store walk that List performs. Returns "" (bucket root) when rootPrefix
// is empty, otherwise "<root>/".
func rootPrefix(p string) string {
	rp := strings.Trim(p, "/")
	if rp == "" {
		return ""
	}
	return rp + "/"
}

// objectKey assembles the full S3 object key for a record using the supplied
// initial-creation timestamp prefix (already formatted to timestampWidth).
func objectKey(rootPrefix string, key fingerprint.Key, timestamp string) string {
	return objectPrefix(rootPrefix, key) + timestamp + "-" + key.InputHash + fingerprint.FileExt
}

// suffixForHash returns the trailing "-<input_hash>.pb" segment used to filter
// timestamp variants of a single Key out of a ListObjectsV2 response.
func suffixForHash(inputHash string) string {
	return "-" + inputHash + fingerprint.FileExt
}

// parseObjectKey reverses objectKey: given the full S3 key and the same
// rootPrefix, recover the (spec_relpath, task_id, input_hash, timestamp).
// Returns ok=false for keys that do not look like a record (foreign objects
// in the bucket are silently skipped, matching the local backend's
// "ignore stray files" stance).
func parseObjectKey(rootPrefix, fullKey string) (spec, task, inputHash, timestamp string, ok bool) {
	if !strings.HasSuffix(fullKey, fingerprint.FileExt) {
		return "", "", "", "", false
	}
	rp := strings.Trim(rootPrefix, "/")
	body := fullKey
	if rp != "" {
		want := rp + "/"
		if !strings.HasPrefix(body, want) {
			return "", "", "", "", false
		}
		body = body[len(want):]
	}
	stem := strings.TrimSuffix(body, fingerprint.FileExt)
	parts := strings.Split(stem, "/")
	if len(parts) < 2 {
		return "", "", "", "", false
	}
	filename := parts[len(parts)-1]
	taskID := parts[len(parts)-2]
	specParts := parts[:len(parts)-2]

	dash := strings.IndexByte(filename, '-')
	if dash <= 0 || dash == len(filename)-1 {
		return "", "", "", "", false
	}
	prefix, hash := filename[:dash], filename[dash+1:]
	if !looksLikeTimestamp(prefix) {
		return "", "", "", "", false
	}
	return strings.Join(specParts, "/"), taskID, hash, prefix, true
}

func looksLikeTimestamp(s string) bool {
	if len(s) != timestampWidth {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// formatTimestamp formats a millisecond-precision UTC timestamp into the
// 17-digit ADR-0010 prefix.
func formatTimestamp(year int, month, day, hour, min, sec, milli int) string {
	return fmt.Sprintf("%04d%02d%02d%02d%02d%02d%03d",
		year, month, day, hour, min, sec, milli)
}
