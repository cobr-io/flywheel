// Package render walks a template directory tree (any fs.FS — disk via
// os.DirFS or an embed.FS) and writes each `.tmpl` file rendered with the
// supplied values into a destination directory on disk. Used by
// `flywheel init` (templates/client-skeleton/) and `flywheel add app`
// (manifests/per-app-template/).
//
// Non-`.tmpl` files are copied verbatim. The destination tree mirrors
// the source tree minus the `.tmpl` suffix.
package render

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"text/template"
)

const tmplSuffix = ".tmpl"

// Tree walks `srcRoot` inside `fsys`, renders every *.tmpl with `values`,
// and writes the output mirror under `destRoot` on disk. Non-tmpl files
// are copied verbatim.
//
// `fsys` may be an embed.FS (production) or `os.DirFS(disk-path)` (tests).
// `srcRoot` is the directory inside the FS to mirror (use "." for the FS
// root).
//
// Existing files at the destination are overwritten. Callers wanting
// merge semantics should use a 3-way diff layer above this package.
func Tree(fsys fs.FS, srcRoot, destRoot string, values any) error {
	return fs.WalkDir(fsys, srcRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := relPath(srcRoot, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		destRel := strings.TrimSuffix(rel, tmplSuffix)
		dest := filepath.Join(destRoot, filepath.FromSlash(destRel))

		if d.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}

		if strings.HasSuffix(p, tmplSuffix) {
			return renderFile(fsys, p, dest, values)
		}
		return copyFile(fsys, p, dest)
	})
}

// relPath returns p relative to root, in fs.FS slash-path form (never
// `..`-escapes since fs.WalkDir only emits descendants of root).
func relPath(root, p string) (string, error) {
	root = path.Clean(root)
	p = path.Clean(p)
	if root == "." {
		return p, nil
	}
	if p == root {
		return ".", nil
	}
	if !strings.HasPrefix(p, root+"/") {
		return "", fmt.Errorf("%s not under %s", p, root)
	}
	return p[len(root)+1:], nil
}

func renderFile(fsys fs.FS, src, dest string, values any) error {
	raw, err := fs.ReadFile(fsys, src)
	if err != nil {
		return err
	}
	tmpl, err := template.New(path.Base(src)).
		Option("missingkey=error").
		Parse(string(raw))
	if err != nil {
		return fmt.Errorf("parse %s: %w", src, err)
	}
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	if err := tmpl.Execute(out, values); err != nil {
		return fmt.Errorf("render %s: %w", src, err)
	}
	// Rendered files default to 0o644 — embed.FS reports 0o444 for every
	// entry, which would make the output read-only to the developer and
	// break commands like `flywheel add app` that mutate the rendered
	// tree afterwards.
	_ = os.Chmod(dest, 0o644)
	return nil
}

func copyFile(fsys fs.FS, src, dest string) error {
	in, err := fsys.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	_ = os.Chmod(dest, 0o644)
	return nil
}
