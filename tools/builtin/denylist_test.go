package builtin

import (
	"slices"
	"strings"
	"testing"
)

// newDenyList builds a DenyList from extra, failing the test on error.
func newDenyList(t *testing.T, extra ...string) DenyList {
	t.Helper()
	d, err := NewDenyList(extra)
	if err != nil {
		t.Fatalf("NewDenyList(%q): %v", extra, err)
	}
	return d
}

// newToolWithDeny creates a read_file tool confined to dir that uses deny, failing the test on error.
func newToolWithDeny(t *testing.T, dir string, deny DenyList) *ReadFileTool {
	t.Helper()
	tool, err := NewReadFileTool(dir, "", testMaxBytes, deny)
	if err != nil {
		t.Fatalf("NewReadFileTool(%q, %v): %v", dir, deny.Patterns(), err)
	}
	return tool
}

// - Verify extra patterns are refused at any depth and in any case, as a file or a directory component,
// and look-alike names are not
func Test_Unit_DenyList_Extras_Denied(t *testing.T) {
	d := newDenyList(t, "*.secret", "credentials.json", "Private*")
	cases := map[string]bool{
		"db.secret": true, "a/b/db.secret": true, "DB.SECRET": true, "x.Secret": true,
		"credentials.json": true, "CREDENTIALS.JSON": true, "sub/credentials.json": true,
		"sub/creds.json/x": false, "sub/credentials.json/x": true, // a directory component matches too
		"private": true, "privatekeys/notes.txt": true, "PRIVATE_stuff.txt": true, // extra given in upper case still matches
		"secret.txt": false, "credentials.json.bak": false, "my-credentials.json": false, "notprivate.txt": false,
		"a.txt": false, "sub/b.txt": false,
	}
	for rel, want := range cases {
		if got := d.IsDenied(rel); got != want {
			t.Errorf("IsDenied(%q) with extras %v = %v, want %v", rel, d.Patterns(), got, want)
		}
	}
}

// - Verify a nested directory component that matches an extra blocks everything below it
func Test_Unit_DenyList_Extras_NestedComponent(t *testing.T) {
	d := newDenyList(t, "creds.json")
	for _, rel := range []string{"sub/creds.json/x", "a/b/CREDS.JSON/c/d.txt", "creds.json"} {
		if !d.IsDenied(rel) {
			t.Errorf("IsDenied(%q) = false, want true for extra creds.json", rel)
		}
	}
	if d.IsDenied("sub/creds.jsonx/x") {
		t.Errorf("IsDenied(%q) = true, want false (not the same name)", "sub/creds.jsonx/x")
	}
}

// - Verify the built-in names stay denied when extras are added: config extends the list, never replaces it
func Test_Unit_DenyList_Extras_KeepBuiltins(t *testing.T) {
	d := newDenyList(t, "*.secret")
	for _, rel := range []string{".env", ".env.local", "key.pem", "server.key", "id_rsa", ".ssh/config", ".agent-trace/x.jsonl", "A/.ENV"} {
		if !d.IsDenied(rel) {
			t.Errorf("IsDenied(%q) = false with extras, want true (built-in)", rel)
		}
	}
}

// - Verify no extras (nil or empty) cannot unblock anything: the built-ins are still denied, same as the zero value
func Test_Unit_DenyList_NoExtras_StillDeniesBuiltins(t *testing.T) {
	for name, extra := range map[string][]string{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			d, err := NewDenyList(extra)
			if err != nil {
				t.Fatalf("NewDenyList(%v): %v", extra, err)
			}
			for _, rel := range []string{".env", "key.pem", ".ssh/config"} {
				if !d.IsDenied(rel) {
					t.Errorf("IsDenied(%q) = false, want true", rel)
				}
			}
			if !slices.Equal(d.Patterns(), DeniedNames()) {
				t.Errorf("Patterns() = %q, want the built-ins %q", d.Patterns(), DeniedNames())
			}
		})
	}
}

// - Verify .env.example stays readable even when an extra pattern would match it
func Test_Unit_DenyList_EnvExample_AlwaysAllowed(t *testing.T) {
	d := newDenyList(t, ".env*", "*.example")
	if d.IsDenied(".env.example") || d.IsDenied("sub/.ENV.EXAMPLE") {
		t.Errorf("expected .env.example to be allowed with extras %v", d.Patterns())
	}
	if !d.IsDenied(".envrc") || !d.IsDenied("app.example") {
		t.Errorf("expected the extras to deny .envrc and app.example")
	}
}

// - Verify each kind of invalid pattern is rejected: empty, whitespace only, containing / or \, malformed path.Match
// syntax, and that one bad entry fails the whole list
func Test_Unit_NewDenyList_InvalidPattern_ReturnsError(t *testing.T) {
	cases := map[string][]string{
		"empty":            {""},
		"whitespace":       {"   "},
		"tab":              {"\t"},
		"slash":            {"sub/creds.json"},
		"leading slash":    {"/etc"},
		"backslash":        {`sub\creds.json`},
		"unclosed bracket": {"[abc"},
		"bracket at end":   {"abc["},
		"bad among good":   {"*.secret", "[abc", "creds.json"},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			d, err := NewDenyList(extra)
			if err == nil {
				t.Fatalf("NewDenyList(%q): expected an error, got patterns %q", extra, d.Patterns())
			}
			if !strings.Contains(err.Error(), "denyFileNames") {
				t.Errorf("expected the error to name denyFileNames, got %q", err.Error())
			}
			if !slices.Equal(d.Patterns(), DeniedNames()) {
				t.Errorf("expected the zero DenyList on error, got patterns %q", d.Patterns())
			}
		})
	}
}

// - Verify Patterns lists the built-ins first, then the extras (lowercased) in the order given
func Test_Unit_DenyList_Patterns_Order(t *testing.T) {
	d := newDenyList(t, "Zeta.TXT", "*.secret", "alpha")
	want := append(DeniedNames(), "zeta.txt", "*.secret", "alpha")
	if got := d.Patterns(); !slices.Equal(got, want) {
		t.Errorf("Patterns() = %q, want %q", got, want)
	}
}

// - Verify Patterns returns a copy: changing the result changes neither the built-ins nor the extras
func Test_Unit_DenyList_Patterns_ReturnsCopy(t *testing.T) {
	d := newDenyList(t, "*.secret")
	got := d.Patterns()
	for i := range got {
		got[i] = "mutated"
	}
	if after := d.Patterns(); after[0] != ".env" || after[len(after)-1] != "*.secret" {
		t.Errorf("Patterns() changed after mutating a previous result: %q", after)
	}
	if !d.IsDenied(".env") || !d.IsDenied("x.secret") {
		t.Errorf("expected .env and x.secret still denied after mutating Patterns()")
	}
	if !slices.Contains(DeniedNames(), ".env") {
		t.Errorf("expected the built-in list unchanged, got %q", DeniedNames())
	}
}

// - Verify read_file refuses a file matching an extra (by relative path, absolute path, other case and via ".."),
// leaks nothing, and names the extra in the error; a file matching no pattern still reads
func Test_ReadFile_DenyFileNames_Blocked(t *testing.T) {
	work := t.TempDir()
	abs := writeFile(t, work, "config/credentials.json", []byte("SECRET-FAKE_SECRET_TOKEN_123"))
	writeFile(t, work, "db.secret", []byte("SECRET-DB"))
	writeFile(t, work, "notes.txt", []byte("ALLOWED-NOTES"))
	tool := newToolWithDeny(t, work, newDenyList(t, "credentials.json", "*.SECRET"))

	for _, p := range []string{"config/credentials.json", abs, "config/../config/credentials.json", "CONFIG/CREDENTIALS.JSON", "db.secret"} {
		t.Run(p, func(t *testing.T) {
			got, err := readPath(tool, p)
			if err == nil {
				t.Fatalf("expected %q to be blocked, got %q", p, got)
			}
			if got != "" || strings.Contains(err.Error(), "SECRET-") {
				t.Errorf("expected nothing leaked, got output %q and error %q", got, err.Error())
			}
			for _, want := range []string{"deny list", "credentials.json", "*.secret", ".env.example is allowed"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("expected %q in the error %q", want, err.Error())
				}
			}
		})
	}
	expectReadable(t, tool, "notes.txt", "ALLOWED-NOTES")
}

// - Verify .env.example is readable through read_file even with an extra ".env*", while .envrc is refused
func Test_ReadFile_DenyFileNames_EnvExampleStillReadable(t *testing.T) {
	work := t.TempDir()
	writeFile(t, work, ".env.example", []byte("ALLOWED-EXAMPLE"))
	writeFile(t, work, ".envrc", []byte("SECRET-ENVRC"))
	tool := newToolWithDeny(t, work, newDenyList(t, ".env*"))

	expectReadable(t, tool, ".env.example", "ALLOWED-EXAMPLE")
	if got, err := readPath(tool, ".envrc"); err == nil || !strings.Contains(err.Error(), "deny list") {
		t.Errorf("expected .envrc to be blocked by the extra, got (%q, %v)", got, err)
	}
}

// - Verify Patterns of a list with no extras (the zero value, or NewDenyList(nil)) is also a copy: changing it can't
// change the built-in list that every read_file shares
func Test_Unit_DenyList_Patterns_NoExtras_ReturnsCopy(t *testing.T) {
	none, err := NewDenyList(nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, d := range map[string]DenyList{"zero value": {}, "NewDenyList(nil)": none} {
		got := d.Patterns()
		got[0] = "mutated"
		if after := (DenyList{}).Patterns()[0]; after != ".env" {
			t.Errorf("%s: Patterns()[0] = %q after mutating a previous result, want .env", name, after)
		}
		if !(DenyList{}).IsDenied(".env") {
			t.Errorf("%s: expected .env still denied after mutating Patterns()", name)
		}
	}
}
