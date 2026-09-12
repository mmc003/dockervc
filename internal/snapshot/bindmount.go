package snapshot

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// tarHostPath archives a host directory or file into a tar stream. Used for
// --include-bind-mounts; only meaningful when dockervc runs on the Docker
// host itself.
func tarHostPath(path string) (io.ReadCloser, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() && !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("unsupported file type %s", fi.Mode())
	}

	pr, pw := io.Pipe()
	go func() {
		err := writeTar(pw, path)
		pw.CloseWithError(err) // nil error → clean EOF
	}()
	return pr, nil
}

func writeTar(w io.WriteCloser, root string) error {
	tw := tar.NewWriter(w)
	defer tw.Close()

	return filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		link := ""
		if fi.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			link = target
		}
		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			rel = "/"
		}
		hdr.Name = rel // paths are relative to the bind-mount root
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			if _, err := io.Copy(tw, f); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
		return nil
	})
}

// HumanBytes formats a byte count for humans.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
