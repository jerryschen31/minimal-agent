package tool

import (
	"bytes"
    "context"
    "encoding/json"
    "fmt"
    "io/fs"
    "os"
    "path/filepath"
    "regexp"
    "strings"
)


func Grep(root string) agent.Tool {
	return Func{
		Name:        "grep",
		Description: "Search file contents for a regular expression. Returns matching lines as path:line: text.",
		Schema: `{ "type":"object",
						"properties":
						  {"pattern": {"type":"string", "description": "RE2 regular expression"}, 
						  "path": {"type":"string", "description": "File path to search, relative to the working directory"},
						  "required": ["pattern"]
						}
				 }`,
		Fn: func(ctx context.Context, raw json.RawMessage) (string, error) {
			// parse input arguments
			in, err := Args[struct{ Pattern string, Path string }](raw)
			if err != nil {
				return "", err
			}
			if in.Path == "" {
				in.Path = "."
			}

			// compile the regular expression pattern to search for
			re, err := regexp.Compile(in.Pattern)
			if err != nil {
                return "", fmt.Errorf("invalid pattern %q: %w", in.Pattern, err)
            }

			// confine the search to the specified root and path
			p, err := confine(root, in.Path)
			if err != nil {
				return "", err
			}

			// walk the directory tree and search for matching lines
			var out strings.Builder
			err = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return nil // unreadable entry: skip it, don't abort the walk
				}

				// read the file contents at path
				b, err := os.ReadFile(path)
				if err != nil || bytes.IndexByte(b, 0) >= 0 { // NUL byte = binary file, skip
					return nil
				}
				// determine the relative file name for output
				name, relErr := filepath.Rel(p, path)
                    if relErr != nil {
                        name = path
                    }
				// search each line of the file for matches to the regular expression
                for i, line := range strings.Split(string(b), "\n") {
                    if re.MatchString(line) {
                        fmt.Fprintf(&out, "%s:%d: %s\n", name, i+1, line)
                    }
                }
                return nil
            })
            if err != nil {
                return "", err
            }
            if out.Len() == 0 {
                return "no matches", nil // an empty string reads as a broken tool
            }
			// return the truncated output of the search results
            return Truncate(out.String(), maxOutput), nil
    	}
	}
}
