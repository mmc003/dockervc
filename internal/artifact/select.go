package artifact

import (
	"fmt"
	"strings"

	"dockervc/internal/model"
)

func SelectVolume(m *model.Manifest, name string) (Selection, model.VolumeRecord, error) {
	for _, rec := range m.Volumes {
		if rec.Name == name {
			return Selection{Kind: "volume", Name: rec.Name, ObjectHash: rec.Object}, rec, nil
		}
	}
	names := make([]string, 0, len(m.Volumes))
	for _, rec := range m.Volumes {
		names = append(names, rec.Name)
	}
	return Selection{}, model.VolumeRecord{}, fmt.Errorf("unknown volume %q (snapshot has: %s)", name, strings.Join(names, ", "))
}

func SelectImage(m *model.Manifest, selector string) (Selection, model.ImageRecord, error) {
	var prefix []model.ImageRecord
	for _, rec := range m.Images {
		for _, ref := range rec.Refs {
			if ref == selector {
				return imageSelection(rec), rec, nil
			}
		}
		if rec.Digest == selector {
			return imageSelection(rec), rec, nil
		}
		if strings.HasPrefix(rec.Digest, selector) {
			prefix = append(prefix, rec)
		}
	}
	if len(prefix) == 1 {
		return imageSelection(prefix[0]), prefix[0], nil
	}
	if len(prefix) > 1 {
		return Selection{}, model.ImageRecord{}, fmt.Errorf("ambiguous image %q matches %d images — use a full digest", selector, len(prefix))
	}
	var names []string
	for _, rec := range m.Images {
		if len(rec.Refs) > 0 {
			names = append(names, rec.Refs...)
		} else {
			names = append(names, rec.Digest)
		}
	}
	return Selection{}, model.ImageRecord{}, fmt.Errorf("unknown image %q (snapshot has: %s)", selector, strings.Join(names, ", "))
}

func imageSelection(rec model.ImageRecord) Selection {
	name := rec.Digest
	if len(rec.Refs) > 0 {
		name = rec.Refs[0]
	}
	return Selection{Kind: "image", Name: name, ObjectHash: rec.Object}
}
