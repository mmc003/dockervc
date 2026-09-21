package cli

import (
	"fmt"
	"sort"
	"strings"

	"dockervc/internal/model"
	"dockervc/internal/snapshot"
	"dockervc/internal/store"
)

func (t *tui) askExportScope(snapshotID string) {
	s, err := store.Open(storePath)
	if err != nil {
		t.flash = "cannot open store: " + err.Error()
		return
	}
	m, err := s.GetSnapshot(snapshotID)
	s.Close()
	if err != nil {
		t.flash = "cannot read snapshot: " + err.Error()
		return
	}
	items := []listItem{{text: "whole snapshot", note: "portable .dvca archive", value: "snapshot"}}
	if len(m.Volumes) > 0 {
		items = append(items,
			listItem{text: "whole volume", note: "standard tar", value: "volume"},
			listItem{text: "file or directory", note: "filtered standard tar", value: "path"},
			listItem{text: "one raw file", note: "file contents without tar", value: "raw"})
	}
	if len(m.Images) > 0 {
		items = append(items, listItem{text: "whole image", note: "Docker-save tar", value: "image"})
	}
	t.askListOne("what should be exported?", items, func(kind string, ok bool) {
		if !ok {
			return
		}
		switch kind {
		case "snapshot":
			t.askPath("output path", defaultExportPath(snapshotID), func(path string, ok bool) {
				if ok && path != "" {
					t.execTUI("export", snapshotID, "-o", path)
				}
			})
		case "volume", "path", "raw":
			t.askExportVolume(m, kind)
		case "image":
			t.askExportImage(m)
		}
	})
}

func (t *tui) askExportVolume(m *model.Manifest, kind string) {
	items := make([]listItem, 0, len(m.Volumes))
	for _, volume := range m.Volumes {
		items = append(items, listItem{text: volume.Name, note: snapshot.HumanBytes(volume.Size), value: volume.Name})
	}
	t.askFilterListOne("which volume?", items, func(volume string, ok bool) {
		if !ok {
			return
		}
		if kind == "volume" {
			t.execTUI("export", m.ID, "--volume", volume)
			return
		}
		t.askExportVolumePath(m.ID, findManifestVolume(m, volume), kind)
	})
}

func findManifestVolume(m *model.Manifest, name string) model.VolumeRecord {
	for _, volume := range m.Volumes {
		if volume.Name == name {
			return volume
		}
	}
	return model.VolumeRecord{Name: name}
}

func (t *tui) askExportVolumePath(snapshotID string, volume model.VolumeRecord, kind string) {
	if volume.IndexObject == "" {
		t.flash = "file index unavailable — enter the path manually"
		t.askManualExportPath(snapshotID, volume.Name, kind)
		return
	}
	s, err := store.Open(storePath)
	if err != nil {
		t.flash = "cannot open store: " + err.Error()
		return
	}
	idx, err := snapshot.LoadFileIndex(s.OpenObject, volume)
	s.Close()
	if err != nil {
		t.flash = "cannot load file index — enter the path manually"
		t.askManualExportPath(snapshotID, volume.Name, kind)
		return
	}
	t.askExportPathLevel(snapshotID, volume.Name, kind, idx, "")
}

func (t *tui) askExportPathLevel(snapshotID, volume, kind string, idx *snapshot.FileIndex, prefix string) {
	items := volumePathItems(idx, prefix, kind == "raw")
	title := volume + ":/"
	if prefix != "" {
		title += prefix + "/"
	}
	t.askFilterListOne("browse "+title, items, func(value string, ok bool) {
		if !ok {
			return
		}
		switch {
		case value == "manual":
			t.askManualExportPath(snapshotID, volume, kind)
		case value == "up":
			parent := ""
			if i := strings.LastIndex(prefix, "/"); i >= 0 {
				parent = prefix[:i]
			}
			t.askExportPathLevel(snapshotID, volume, kind, idx, parent)
		case strings.HasPrefix(value, "dir:"):
			t.askExportPathLevel(snapshotID, volume, kind, idx, strings.TrimPrefix(value, "dir:"))
		case strings.HasPrefix(value, "select:"):
			t.runSelectedPathExport(snapshotID, volume, strings.TrimPrefix(value, "select:"), kind)
		}
	})
}

func (t *tui) askManualExportPath(snapshotID, volume, kind string) {
	t.askText("path inside volume", "", func(path string, ok bool) {
		if ok && path != "" {
			t.runSelectedPathExport(snapshotID, volume, path, kind)
		}
	})
}

func (t *tui) runSelectedPathExport(snapshotID, volume, path, kind string) {
	argv := []string{"export", snapshotID, "--volume", volume, "--path", path}
	if kind == "raw" {
		argv = append(argv, "--raw")
	}
	t.execTUI(argv...)
}

type pathBrowserEntry struct {
	name  string
	path  string
	dir   bool
	files int
	bytes int64
	meta  snapshot.FileMeta
}

func volumePathItems(idx *snapshot.FileIndex, prefix string, raw bool) []listItem {
	byName := map[string]*pathBrowserEntry{}
	if idx != nil {
		for name, meta := range idx.Files {
			remainder := name
			if prefix != "" {
				if !strings.HasPrefix(name, prefix+"/") {
					continue
				}
				remainder = strings.TrimPrefix(name, prefix+"/")
			}
			part, rest, nested := strings.Cut(remainder, "/")
			if part == "" {
				continue
			}
			entry := byName[part]
			if entry == nil {
				full := part
				if prefix != "" {
					full = prefix + "/" + part
				}
				entry = &pathBrowserEntry{name: part, path: full}
				byName[part] = entry
			}
			if nested && rest != "" {
				entry.dir = true
				entry.files++
				if meta.Type == "file" {
					entry.bytes += meta.Size
				}
			} else {
				entry.meta = meta
			}
		}
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	items := make([]listItem, 0, len(names)+3)
	if prefix != "" {
		if !raw {
			items = append(items, listItem{text: "export this folder", note: prefix, value: "select:" + prefix})
		}
		items = append(items, listItem{text: "../", note: "parent folder", value: "up"})
	}
	for _, name := range names {
		entry := byName[name]
		if entry.dir {
			note := fmt.Sprintf("folder · %d file(s) · %s", entry.files, snapshot.HumanBytes(entry.bytes))
			items = append(items, listItem{text: entry.name + "/", note: note, value: "dir:" + entry.path})
			continue
		}
		if raw && entry.meta.Type != "file" {
			continue
		}
		note := entry.meta.Type
		if entry.meta.Type == "file" {
			note += " · " + snapshot.HumanBytes(entry.meta.Size)
		} else if entry.meta.Link != "" {
			note += " → " + entry.meta.Link
		}
		items = append(items, listItem{text: entry.name, note: note, value: "select:" + entry.path})
	}
	items = append(items, listItem{text: "enter path manually…", note: "for empty or unindexed folders", value: "manual"})
	return items
}

func (t *tui) askExportImage(m *model.Manifest) {
	items := make([]listItem, 0, len(m.Images))
	for _, image := range m.Images {
		name := image.Digest
		if len(image.Refs) > 0 {
			name = image.Refs[0]
		}
		items = append(items, listItem{text: name, note: fmt.Sprintf("%s · %s", snapshot.HumanBytes(image.Size), image.Digest), value: name})
	}
	t.askFilterListOne("which image?", items, func(image string, ok bool) {
		if ok {
			t.execTUI("export", m.ID, "--image", image)
		}
	})
}

func (t *tui) askLocalVolumeFiles(snapshotID string) {
	s, err := store.Open(storePath)
	if err != nil {
		t.flash = "cannot open store: " + err.Error()
		return
	}
	m, err := s.GetSnapshot(snapshotID)
	s.Close()
	if err != nil {
		t.flash = "cannot read snapshot: " + err.Error()
		return
	}
	items := make([]listItem, 0, len(m.Volumes))
	for _, volume := range m.Volumes {
		note := fmt.Sprintf("%s · %d indexed file(s)", snapshot.HumanBytes(volume.Size), volume.Files)
		if volume.IndexObject == "" {
			note = "file index unavailable"
		}
		items = append(items, listItem{text: volume.Name, note: note, value: volume.Name})
	}
	if len(items) == 0 {
		t.flash = "snapshot contains no volumes"
		return
	}
	t.askListOne("browse which volume?", items, func(volume string, ok bool) {
		if !ok {
			return
		}
		t.askText("optional file/directory prefix", "", func(prefix string, ok bool) {
			if !ok {
				return
			}
			argv := []string{"files", snapshotID, volume}
			if prefix != "" {
				argv = append(argv, prefix)
			}
			t.execTUI(argv...)
		})
	})
}
