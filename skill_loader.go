package main

import (
	"os"
	"path/filepath"
	"sort"
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

		// skill name
		name := filepath.Base(filepath.Dir(path))
		l.Skills[name] = Skill{
			Meta: make(map[string]string),
			Body: string(data),
			Path: path,
		}
	}
	return nil
}
