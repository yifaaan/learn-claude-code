package main

import (
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
