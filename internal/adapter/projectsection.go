package adapter

import (
	"regexp"
	"strings"
)

// Install-rooted agents (hermes, openclaw) read ONE central memory file and
// never look inside project folders, so project-scoped memory written there
// is invisible to them. These helpers materialize a project memory artifact
// INTO the central file as a delimited, deterministically-keyed section:
//
//	<!-- aplexica:project-memory:<key> -->
//	## Project: <title>
//	<content>
//	<!-- /aplexica:project-memory:<key> -->
//
// UpsertProjectSection replaces the keyed section in place (or appends it),
// and StripProjectSections removes every such section — the import side runs
// it so project mirrors never leak into the agent's GLOBAL memory artifact.
// Sections are read-only mirrors: hand-edits inside one are overwritten by
// the next export of that project artifact.

var projectSectionRe = regexp.MustCompile(`(?s)<!-- aplexica:project-memory:([^ ]+) -->\n.*?<!-- /aplexica:project-memory:[^ ]+ -->\n?`)

func projectSectionBlock(key, title, content string) string {
	body := strings.TrimRight(content, "\n")
	return "<!-- aplexica:project-memory:" + key + " -->\n" +
		"## Project: " + title + "\n\n" +
		body + "\n" +
		"<!-- /aplexica:project-memory:" + key + " -->\n"
}

// UpsertProjectSection returns existing with the section for key replaced by
// the given title+content, appending it (blank-line separated) when absent.
// Idempotent: same inputs always produce the same output.
func UpsertProjectSection(existing, key, title, content string) string {
	block := projectSectionBlock(key, title, content)
	replaced := false
	out := projectSectionRe.ReplaceAllStringFunc(existing, func(m string) string {
		sub := projectSectionRe.FindStringSubmatch(m)
		if len(sub) > 1 && sub[1] == key && !replaced {
			replaced = true
			return block
		}
		return m
	})
	if replaced {
		return out
	}
	if strings.TrimSpace(existing) == "" {
		return block
	}
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out + "\n" + block
}

// StripProjectSections removes every aplexica project-memory section,
// collapsing the blank runs the removal leaves to at most one blank line.
// Inverse of the upsert for the global content around the sections:
// StripProjectSections(UpsertProjectSection(base, …)) round-trips base
// modulo a single trailing newline.
func StripProjectSections(content string) string {
	out := projectSectionRe.ReplaceAllString(content, "")
	for strings.Contains(out, "\n\n\n") {
		out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	}
	return strings.TrimRight(out, "\n") + ensureNL(out)
}

func ensureNL(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return "\n"
}
