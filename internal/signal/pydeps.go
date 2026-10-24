package signal

import (
	"bufio"
	"bytes"
	"regexp"
	"strings"
)

// pyName normalises a Python package name: lowercase, "_" as "-".
func pyName(s string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "_", "-")
}

var pyLead = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*`)

// pySpec returns the package name at the start of a requirement such as
// "Fastapi[all]>=0.1; python_version>'3'", or "".
func pySpec(spec string) string {
	return pyName(pyLead.FindString(strings.TrimSpace(spec)))
}

// PythonRequirements lists package names in a requirements.txt.
func PythonRequirements(b []byte) []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "-") {
			continue
		}
		if n := pySpec(line); n != "" {
			out = append(out, n)
		}
	}
	return out
}

var quoted = regexp.MustCompile(`"([^"]*)"|'([^']*)'`)

// PyprojectDeps lists package names from [project] dependencies and
// [tool.poetry.dependencies] in a pyproject.toml. It reads just those two
// places, with a line scanner rather than a full TOML parser.
func PyprojectDeps(b []byte) []string {
	var out []string
	section := ""
	inList := false
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !inList && strings.HasPrefix(line, "[") {
			section = strings.Trim(line, "[] ")
			continue
		}
		switch {
		case section == "project":
			if !inList {
				k, v, ok := strings.Cut(line, "=")
				if !ok || strings.TrimSpace(k) != "dependencies" {
					continue
				}
				line, inList = v, true
			}
			for _, m := range quoted.FindAllStringSubmatch(line, -1) {
				if n := pySpec(m[1] + m[2]); n != "" {
					out = append(out, n)
				}
			}
			if strings.Contains(line, "]") {
				inList = false
			}
		case section == "tool.poetry.dependencies":
			k, _, ok := strings.Cut(line, "=")
			if n := pyName(k); ok && n != "" && n != "python" {
				out = append(out, n)
			}
		}
	}
	return out
}
