package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Skill struct {
	Meta map[string]string
	Body string
	Path string
}

type SkillLoader struct {
	SkillsDir string
	Skills    map[string]Skill
}

func NewSkillLoader(skillDir string) (*SkillLoader, error) {
	loader := &SkillLoader{
		SkillsDir: skillDir,
		Skills:    make(map[string]Skill),
	}
	if err := loader.loadAll(); err != nil {
		return nil, err
	}
	return loader, nil
}

// loadAll 扫描 skills/<name>/SKILL.md 文件并加载所有skill
func (l *SkillLoader) loadAll() error {
	if _, err := os.Stat(l.SkillsDir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	matches, err := filepath.Glob(filepath.Join(l.SkillsDir, "*", "SKILL.md"))
	if err != nil {
		return err
	}

	sort.Strings(matches)
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		meta, body := parseFrontmatter(string(data))
		// skill name
		name := strings.TrimSpace(meta["name"])
		if name == "" {
			name = filepath.Base(filepath.Dir(path))
		}
		l.Skills[name] = Skill{
			Meta: meta,
			Body: body,
			Path: path,
		}
	}
	return nil
}

func parseFrontmatter(text string) (map[string]string, string) {
	meta := map[string]string{}

	if !strings.HasPrefix(text, "---\n") {
		return meta, strings.TrimSpace(text)
	}

	rest := strings.TrimPrefix(text, "---\n")
	idx := strings.Index(rest, "\n---\n")
	if idx == -1 {
		return meta, strings.TrimSpace(text)
	}

	header := rest[:idx]
	body := rest[idx+5:]

	for _, rawLine := range strings.Split(header, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}

		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}

		meta[strings.TrimSpace(key)] = strings.TrimSpace(val)
	}
	return meta, strings.TrimSpace(body)
}

func (l *SkillLoader) Descriptions() string {
	if len(l.Skills) == 0 {
		return "(no skills available)"
	}

	names := make([]string, 0, len(l.Skills))
	for name := range l.Skills {
		names = append(names, name)
	}
	sort.Strings(names)

	lines := make([]string, 0, len(names))
	for _, name := range names {
		skill := l.Skills[name]
		desc := skill.Meta["description"]
		if desc == "" {
			desc = "No description"
		}
		tags := strings.TrimSpace(skill.Meta["tags"])
		line := fmt.Sprintf("  - %s: %s", name, desc)
		if tags != "" {
			line += fmt.Sprintf(" [%s]", tags)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func (l *SkillLoader) Content(name string) string {
	skill, ok := l.Skills[name]
	if !ok {
		names := make([]string, 0, len(l.Skills))
		for n := range l.Skills {
			names = append(names, n)
		}
		sort.Strings(names)

		return fmt.Sprintf(
			"Error: Unknown skill %q. Available: %s",
			name,
			strings.Join(names, ", "),
		)
	}
	return fmt.Sprintf("<skill name=%q>\n%s\n</skill>", name, skill.Body)
}
