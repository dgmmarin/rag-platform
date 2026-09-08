package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// TestAdminPasswordFromEnvOrStdin covers the two non-TTY paths: the env var
// short-circuits stdin entirely, and stdin is read as one line (including the
// EOF-without-trailing-newline case, which must not be treated as an error).
// The TTY prompt branch (isTerminal) needs a real character device and is not
// unit-testable here; it is exercised manually (see admin.go's ponytail note).
func TestAdminPasswordFromEnvOrStdin(t *testing.T) {
	cases := []struct {
		name   string
		envSet bool
		envVal string
		stdin  string
		want   string
	}{
		{
			name:   "env var set wins over stdin",
			envSet: true,
			envVal: "from-env",
			stdin:  "from-stdin\n",
			want:   "from-env",
		},
		{
			name:  "env var unset reads one line from stdin",
			stdin: "from-stdin\n",
			want:  "from-stdin",
		},
		{
			name:  "trailing CRLF is trimmed",
			stdin: "from-stdin\r\n",
			want:  "from-stdin",
		},
		{
			name:  "empty stdin (EOF, no newline) yields empty password, no error",
			stdin: "",
			want:  "",
		},
		{
			name:  "unterminated line (EOF before newline) is still read",
			stdin: "no-newline",
			want:  "no-newline",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.envSet {
				t.Setenv("RAGCTL_ADMIN_PASSWORD", tc.envVal)
			} else {
				// Ensure the var is genuinely absent regardless of ambient env,
				// restoring whatever was there (if anything) after the subtest.
				old, existed := os.LookupEnv("RAGCTL_ADMIN_PASSWORD")
				_ = os.Unsetenv("RAGCTL_ADMIN_PASSWORD")
				t.Cleanup(func() {
					if existed {
						_ = os.Setenv("RAGCTL_ADMIN_PASSWORD", old)
					}
				})
			}

			var stderr bytes.Buffer
			got, err := adminPasswordFromEnvOrStdin(strings.NewReader(tc.stdin), &stderr)
			if err != nil {
				t.Fatalf("adminPasswordFromEnvOrStdin: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got password %q, want %q", got, tc.want)
			}
		})
	}
}
