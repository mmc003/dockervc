package artifact

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"testing"

	"dockervc/internal/store"
)

func testTar(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if err := tw.WriteHeader(&tar.Header{Name: "./", Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"etc/config.txt":   "configuration\n",
		"etc/nested/a.txt": "nested\n",
		"data.bin":         "data",
	} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func testExporter(t *testing.T, raw []byte) (*Exporter, string) {
	t.Helper()
	st, err := store.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	obj, err := st.PutBlob("volume", "", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return &Exporter{Store: st}, obj.Hash
}

func TestExportWholeObject(t *testing.T) {
	raw := testTar(t)
	exp, hash := testExporter(t, raw)
	var out bytes.Buffer
	res, err := exp.Export(context.Background(), Selection{Kind: "volume", Name: "demo", ObjectHash: hash}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), raw) || res.Bytes != int64(len(raw)) || res.SHA256 == "" {
		t.Fatalf("unexpected whole export: bytes=%d hash=%q equal=%v", res.Bytes, res.SHA256, bytes.Equal(out.Bytes(), raw))
	}
}

func TestExportDirectorySelection(t *testing.T) {
	exp, hash := testExporter(t, testTar(t))
	var out bytes.Buffer
	res, err := exp.Export(context.Background(), Selection{Kind: "volume", Name: "demo", ObjectHash: hash, Path: "etc"}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 2 {
		t.Fatalf("files=%d, want 2", res.Files)
	}
	tr := tar.NewReader(&out)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	if got := stringsJoin(names); got != "etc/config.txt,etc/nested/a.txt" && got != "etc/nested/a.txt,etc/config.txt" {
		t.Fatalf("selected names = %q", got)
	}
}

func TestExportRawFile(t *testing.T) {
	exp, hash := testExporter(t, testTar(t))
	var out bytes.Buffer
	res, err := exp.Export(context.Background(), Selection{Kind: "volume", Name: "demo", ObjectHash: hash, Path: "etc/config.txt", Raw: true}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "configuration\n" || res.Files != 1 {
		t.Fatalf("raw output=%q files=%d", out.String(), res.Files)
	}
}

func TestExportRejectsBadSelectionAndCorruption(t *testing.T) {
	exp, hash := testExporter(t, testTar(t))
	for _, p := range []string{"../secret", "/absolute", `C:\secret`} {
		if _, err := exp.Export(context.Background(), Selection{Kind: "volume", Name: "demo", ObjectHash: hash, Path: p}, io.Discard); err == nil {
			t.Fatalf("path %q was accepted", p)
		}
	}
	if _, err := exp.Export(context.Background(), Selection{Kind: "volume", Name: "demo", ObjectHash: hash, Path: "missing"}, io.Discard); err == nil {
		t.Fatal("missing path was accepted")
	}
	b, err := os.ReadFile(exp.Store.ObjectPath(hash))
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-1] ^= 0xff
	if err := os.WriteFile(exp.Store.ObjectPath(hash), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := exp.Export(context.Background(), Selection{Kind: "volume", Name: "demo", ObjectHash: hash}, io.Discard); err == nil {
		t.Fatal("corrupt object was accepted")
	}
}

func stringsJoin(values []string) string {
	var b bytes.Buffer
	for i, value := range values {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(value)
	}
	return b.String()
}
