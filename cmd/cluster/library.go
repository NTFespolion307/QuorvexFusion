package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/NTFespolion307/QuorvexFusion/internal/controller"
	"github.com/NTFespolion307/QuorvexFusion/internal/files"
	"github.com/NTFespolion307/QuorvexFusion/internal/store"
)

// cluster files: the controller's file library, usable from any computer
// with the CLI configured (cluster login).

func listLibrary(folder string) ([]*store.LibraryFile, error) {
	var list []*store.LibraryFile
	err := call("GET", "/api/v1/files?path="+url.QueryEscape(strings.Trim(folder, "/")), nil, &list)
	return list, err
}

// libraryInputs resolves --file PATH[:DEST] to job inputs (no upload).
func libraryInputs(args []string) ([]controller.InputSpec, error) {
	var out []controller.InputSpec
	for _, a := range args {
		src, dest := splitSrcDest(a)
		src = strings.Trim(src, "/")
		list, err := listLibrary(src)
		if err != nil {
			return nil, err
		}
		if len(list) == 0 {
			return nil, fmt.Errorf("--file %s: not in the file library (see: cluster files ls)", src)
		}
		for _, f := range list {
			target := f.Path
			if dest != "" {
				if f.Path == src {
					target = dest
				} else {
					target = path.Join(dest, strings.TrimPrefix(f.Path, src+"/"))
				}
			}
			out = append(out, controller.InputSpec{Path: target, SHA256: f.SHA256, Size: f.Size, Mode: 0o644})
		}
	}
	return out, nil
}

func filesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "files",
		Short: "The file library: upload once, use in many jobs (--file)",
		Long: `The file library keeps files on the controller so jobs can use them as
inputs without uploading them again: cluster submit --file PATH ...
Folders are path prefixes. Works from any computer where the CLI is
logged in (cluster login), and from the web UI's Files page.`,
	}

	ls := &cobra.Command{
		Use:     "ls [folder]",
		Aliases: []string{"list"},
		Short:   "List library files",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			folder := ""
			if len(args) == 1 {
				folder = args[0]
			}
			list, err := listLibrary(folder)
			if err != nil {
				return err
			}
			if globalFlags.json {
				return printJSON(list)
			}
			if len(list) == 0 {
				fmt.Fprintln(os.Stderr, "No files. Upload with: cluster files upload LOCAL [--to FOLDER]")
				return nil
			}
			t := newTable()
			fmt.Fprintln(t, "PATH\tSIZE\tUPLOADED")
			var total int64
			for _, f := range list {
				total += f.Size
				fmt.Fprintf(t, "%s\t%s\t%s\n", f.Path, humanBytes(uint64(f.Size)), f.UploadedAt.Local().Format("2006-01-02 15:04"))
			}
			fmt.Fprintf(t, "\t%s\t(%d files)\n", humanBytes(uint64(total)), len(list))
			return t.Flush()
		},
	}

	var to string
	upload := &cobra.Command{
		Use:   "upload LOCAL... [--to FOLDER]",
		Short: "Upload files or folders to the library (resumable)",
		Example: `  cluster files upload scene.blend textures/ --to city
  cluster files upload datasets/imagenet-small`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			local, err := collectInputs(args)
			if err != nil {
				return err
			}
			prefix := strings.Trim(filepath.ToSlash(to), "/")
			for i := range local {
				if prefix != "" {
					local[i].spec.Path = prefix + "/" + local[i].spec.Path
				}
				if local[i].spec.Path, err = files.CleanRel(local[i].spec.Path); err != nil {
					return err
				}
			}
			specs, err := uploadInputs(local)
			if err != nil {
				return err
			}
			for _, s := range specs {
				if err := call("POST", "/api/v1/files", map[string]string{"path": s.Path, "sha256": s.SHA256}, nil); err != nil {
					return fmt.Errorf("%s: %w", s.Path, err)
				}
			}
			fmt.Printf("Added %d file(s) to the library.\n", len(specs))
			return nil
		},
	}
	upload.Flags().StringVar(&to, "to", "", "library folder to put them in")

	var dir string
	get := &cobra.Command{
		Use:   "get PATH [-o DIR]",
		Short: "Download a library file or folder (resumable)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			src := strings.Trim(args[0], "/")
			list, err := listLibrary(src)
			if err != nil {
				return err
			}
			if len(list) == 0 {
				return fmt.Errorf("%s is not in the library", src)
			}
			c, err := apiClient()
			if err != nil {
				return err
			}
			var total int64
			for _, f := range list {
				total += f.Size
			}
			prog := newProgress(fmt.Sprintf("Downloading %d file(s)", len(list)), total)
			base := path.Dir(src) // keep the selected file/folder name itself
			for _, f := range list {
				rel := strings.TrimPrefix(f.Path, base+"/")
				if base == "." {
					rel = f.Path
				}
				dest, err := files.Join(dir, rel)
				if err != nil {
					return err
				}
				key := f.Path
				if err := c.Download(context.Background(), "/api/v1/files/content?path="+url.QueryEscape(f.Path), dest,
					f.SHA256, f.Size, func(n int64) { prog.set(key, n) }); err != nil {
					fmt.Fprintln(os.Stderr)
					return fmt.Errorf("%s: %w", f.Path, err)
				}
			}
			prog.finish()
			return nil
		},
	}
	get.Flags().StringVarP(&dir, "dir", "o", ".", "directory to save into")

	rm := &cobra.Command{
		Use:   "rm PATH...",
		Short: "Delete library files or folders",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, p := range args {
				var res struct {
					Deleted int `json:"deleted"`
				}
				if err := call("DELETE", "/api/v1/files?path="+url.QueryEscape(strings.Trim(p, "/")), nil, &res); err != nil {
					return fmt.Errorf("%s: %w", p, err)
				}
				fmt.Printf("%s: %d file(s) deleted\n", p, res.Deleted)
			}
			return nil
		},
	}
	cmd.AddCommand(ls, upload, get, rm)
	return cmd
}
