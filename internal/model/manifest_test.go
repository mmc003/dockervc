package model

import (
	"encoding/json"
	"testing"
)

func TestContainerRecipeJSONRoundTrip(t *testing.T) {
	want := Manifest{
		Containers: []ContainerRecord{{
			Name: "app", ImageRef: "example/app:1", ImageID: "sha256:abc",
			ImageKey: "sha256:abc", ConfigHash: "config-hash", Running: true,
		}},
		Images: []ImageRecord{{
			ID: "sha256:abc", Refs: []string{"example/app:1"},
			Digests: []string{"example/app@sha256:def"}, Key: "sha256:abc",
			Digest: "example/app@sha256:def", Object: "object-hash",
		}},
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got Manifest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Containers) != 1 || got.Containers[0].ImageKey != "sha256:abc" ||
		got.Containers[0].ImageObject != "" || len(got.Images) != 1 || got.Images[0].ID != "sha256:abc" {
		t.Fatalf("recipe did not round-trip: %+v", got)
	}
}

func TestLegacyContainerManifestStillDecodes(t *testing.T) {
	data := []byte(`{"containers":[{"name":"old","inspect_json":{},"image_object":"legacy-object","layer_hash":"legacy-layer","running":true,"size":42}],"images":[{"refs":["old:latest"],"digest":"sha256:old","object":"image-object","size":12}]}`)
	var got Manifest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Containers) != 1 || got.Containers[0].ImageObject != "legacy-object" ||
		got.Containers[0].ImageKey != "" || len(got.Images) != 1 || got.Images[0].Key != "" {
		t.Fatalf("legacy manifest compatibility lost: %+v", got)
	}
}
