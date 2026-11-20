package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
)

// renovateConfig is the part of renovate.json devtool reads.
type renovateConfig struct {
	IgnorePaths    []string        `json:"ignorePaths"`
	CustomManagers []customManager `json:"customManagers"`
}

// customManager is one of renovate.json's regex custom managers. Every
// one whose datasourceTemplate is "docker" is a Helper image (ADR 0082).
type customManager struct {
	Description         string   `json:"description"`
	ManagerFilePatterns []string `json:"managerFilePatterns"`
	MatchStrings        []string `json:"matchStrings"`
	DepNameTemplate     string   `json:"depNameTemplate"`
	DatasourceTemplate  string   `json:"datasourceTemplate"`
}

// readRenovate reads renovate.json in the repository at root.
func readRenovate(root string) (renovateConfig, error) {
	var c renovateConfig
	b, err := os.ReadFile(filepath.Join(root, "renovate.json"))
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("renovate.json: %w", err)
	}
	return c, nil
}

// tags applies m's matchStrings to each of its files under root, as
// Renovate does, and returns the currentValue each finds, with the files
// that hold it, and the files that hold none.
func (m customManager) tags(root string) (tags map[string][]string, empty []string, err error) {
	tags = map[string][]string{}
	for _, f := range m.ManagerFilePatterns {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", m.DepNameTemplate, err)
		}
		found := false
		for _, s := range m.MatchStrings {
			re, err := regexp.Compile(s)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", m.DepNameTemplate, err)
			}
			i := re.SubexpIndex("currentValue")
			if i < 0 {
				return nil, nil, fmt.Errorf("%s: matchStrings entry %q has no currentValue group", m.DepNameTemplate, s)
			}
			for _, sm := range re.FindAllStringSubmatch(string(b), -1) {
				if !slices.Contains(tags[sm[i]], f) {
					tags[sm[i]] = append(tags[sm[i]], f)
				}
				found = true
			}
		}
		if !found {
			empty = append(empty, f)
		}
	}
	return tags, empty, nil
}

// helperImageList returns every Helper image of the repository at root,
// sorted: the reference each docker custom manager in renovate.json
// resolves to. A manager whose files hold no tag, or disagree on it, is
// an error, as it is in TestRenovateHelperImagesListed.
func helperImageList(root string) ([]string, error) {
	c, err := readRenovate(root)
	if err != nil {
		return nil, err
	}
	var refs []string
	for _, m := range c.CustomManagers {
		if m.DatasourceTemplate != "docker" {
			continue
		}
		tags, empty, err := m.tags(root)
		if err != nil {
			return nil, err
		}
		if len(empty) > 0 {
			return nil, fmt.Errorf("%s: %v hold no tag", m.DepNameTemplate, empty)
		}
		if len(tags) != 1 {
			return nil, fmt.Errorf("%s: listed files disagree on the tag: %v", m.DepNameTemplate, tags)
		}
		for v := range tags {
			refs = append(refs, m.DepNameTemplate+":"+v)
		}
	}
	slices.Sort(refs)
	return slices.Compact(refs), nil
}

// runHelperImages prints every Helper image, one per line.
func runHelperImages([]string) error {
	refs, err := helperImageList(".")
	if err != nil {
		return err
	}
	for _, r := range refs {
		fmt.Println(r)
	}
	return nil
}
