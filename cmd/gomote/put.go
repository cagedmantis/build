// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/build/internal/gomote/protos"
	"golang.org/x/build/tarutil"
	"golang.org/x/sync/errgroup"
)

// legacyPutTar a .tar.gz
func legacyPutTar(args []string) error {
	if activeGroup != nil {
		return fmt.Errorf("command does not support groups")
	}

	fs := flag.NewFlagSet("put", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "puttar usage: gomote puttar [put-opts] <buildlet-name> [tar.gz file or '-' for stdin]")
		fs.PrintDefaults()
		os.Exit(1)
	}
	var rev string
	fs.StringVar(&rev, "gorev", "", "If non-empty, git hash to download from gerrit and put to the buildlet. e.g. 886b02d705ff for Go 1.4.1. This just maps to the --URL flag, so the two options are mutually exclusive.")
	var dir string
	fs.StringVar(&dir, "dir", "", "relative directory from buildlet's work dir to extra tarball into")
	var tarURL string
	fs.StringVar(&tarURL, "url", "", "URL of tarball, instead of provided file.")

	fs.Parse(args)
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fs.Usage()
	}
	if rev != "" {
		if tarURL != "" {
			fmt.Fprintln(os.Stderr, "--gorev and --url are mutually exclusive")
			fs.Usage()
		}
		tarURL = "https://go.googlesource.com/go/+archive/" + rev + ".tar.gz"
	}

	name := fs.Arg(0)
	bc, err := remoteClient(name)
	if err != nil {
		return err
	}

	ctx := context.Background()

	if tarURL != "" {
		if fs.NArg() != 1 {
			fs.Usage()
		}
		if err := bc.PutTarFromURL(ctx, tarURL, dir); err != nil {
			return err
		}
		if rev != "" {
			// Put a VERSION file there too, to avoid git usage.
			version := strings.NewReader("devel " + rev)
			var vtar tarutil.FileList
			vtar.AddRegular(&tar.Header{
				Name: "VERSION",
				Mode: 0644,
				Size: int64(version.Len()),
			}, int64(version.Len()), version)
			tgz := vtar.TarGz()
			defer tgz.Close()
			return bc.PutTar(ctx, tgz, dir)
		}
		return nil
	}

	var tgz io.Reader = os.Stdin
	if fs.NArg() == 2 && fs.Arg(1) != "-" {
		f, err := os.Open(fs.Arg(1))
		if err != nil {
			return err
		}
		defer f.Close()
		tgz = f
	}
	return bc.PutTar(ctx, tgz, dir)
}

// putTar a .tar.gz
func putTar(args []string) error {
	fs := flag.NewFlagSet("put", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "puttar usage: gomote puttar [put-opts] [instance] <source>")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "<source> may be one of:")
		fmt.Fprintln(os.Stderr, "- A path to a local .tar.gz file.")
		fmt.Fprintln(os.Stderr, "- A URL that points at a .tar.gz file.")
		fmt.Fprintln(os.Stderr, "- The '-' character to indicate a .tar.gz file passed via stdin.")
		fmt.Fprintln(os.Stderr, "- Git hash (min 7 characters) for the Go repository (extract a .tar.gz of the repository at that commit w/o history)")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Instance name is optional if a group is specified.")
		fs.PrintDefaults()
		os.Exit(1)
	}
	var dir string
	fs.StringVar(&dir, "dir", "", "relative directory from buildlet's work dir to extra tarball into")

	fs.Parse(args)

	// Parse arguments.
	var putSet []string
	var src string
	switch fs.NArg() {
	case 1:
		// Must be just the source, so we need an active group.
		if activeGroup == nil {
			fmt.Fprintln(os.Stderr, "no active group found; need an active group with only 1 argument")
			fs.Usage()
		}
		for _, inst := range activeGroup.Instances {
			putSet = append(putSet, inst)
		}
		src = fs.Arg(0)
	case 2:
		// Instance and source is specified.
		putSet = []string{fs.Arg(0)}
		src = fs.Arg(1)
	case 0:
		fmt.Fprintln(os.Stderr, "error: not enough arguments")
		fs.Usage()
	default:
		fmt.Fprintln(os.Stderr, "error: too many arguments")
		fs.Usage()
	}

	// Interpret source.
	var putTarFn func(ctx context.Context, inst string) error
	if src == "-" {
		// We might have multiple readers, so slurp up STDIN
		// and store it, then hand out bytes.Readers to everyone.
		var buf bytes.Buffer
		_, err := io.Copy(&buf, os.Stdin)
		if err != nil {
			return fmt.Errorf("reading stdin: %v", err)
		}
		sharedTarBuf := buf.Bytes()
		putTarFn = func(ctx context.Context, inst string) error {
			return doPutTar(ctx, inst, dir, bytes.NewReader(sharedTarBuf))
		}
	} else {
		u, err := url.Parse(src)
		if err != nil {
			// The URL parser should technically accept any of these, so the fact that
			// we failed means its *very* malformed.
			return fmt.Errorf("malformed source: not a path, a URL, -, or a git hash")
		}
		if u.Scheme != "" || u.Host != "" {
			// Probably a real URL.
			putTarFn = func(ctx context.Context, inst string) error {
				return doPutTarURL(ctx, inst, dir, u.String())
			}
		} else {
			// Probably a path. Check if it exists.
			_, err := os.Stat(src)
			if os.IsNotExist(err) {
				// It must be a git hash. Check if this actually matches a git hash.
				if len(src) < 7 || len(src) > 40 || regexp.MustCompile("[^a-f0-9]").MatchString(src) {
					return fmt.Errorf("malformed source: not a path, a URL, -, or a git hash")
				}
				putTarFn = func(ctx context.Context, inst string) error {
					return doPutTarGoRev(ctx, inst, dir, src)
				}
			} else if err != nil {
				return fmt.Errorf("failed to stat %q: %v", src, err)
			} else {
				// It's a path.
				putTarFn = func(ctx context.Context, inst string) error {
					f, err := os.Open(src)
					if err != nil {
						return fmt.Errorf("opening %q: %v", src, err)
					}
					defer f.Close()
					return doPutTar(ctx, inst, dir, f)
				}
			}
		}
	}
	eg, ctx := errgroup.WithContext(context.Background())
	for _, inst := range putSet {
		inst := inst
		eg.Go(func() error {
			return putTarFn(ctx, inst)
		})
	}
	return eg.Wait()
}

func doPutTarURL(ctx context.Context, name, dir, tarURL string) error {
	client := gomoteServerClient(ctx)
	_, err := client.WriteTGZFromURL(ctx, &protos.WriteTGZFromURLRequest{
		GomoteId:  name,
		Directory: dir,
		Url:       tarURL,
	})
	if err != nil {
		return fmt.Errorf("unable to write tar to instance: %s", statusFromError(err))
	}
	return nil
}

func doPutTarGoRev(ctx context.Context, name, dir, rev string) error {
	tarURL := "https://go.googlesource.com/go/+archive/" + rev + ".tar.gz"
	if err := doPutTarURL(ctx, name, dir, tarURL); err != nil {
		return err
	}

	// Put a VERSION file there too, to avoid git usage.
	version := strings.NewReader("devel " + rev)
	var vtar tarutil.FileList
	vtar.AddRegular(&tar.Header{
		Name: "VERSION",
		Mode: 0644,
		Size: int64(version.Len()),
	}, int64(version.Len()), version)
	tgz := vtar.TarGz()
	defer tgz.Close()

	client := gomoteServerClient(ctx)
	resp, err := client.UploadFile(ctx, &protos.UploadFileRequest{})
	if err != nil {
		return fmt.Errorf("unable to request credentials for a file upload: %s", statusFromError(err))
	}
	if err := uploadToGCS(ctx, resp.GetFields(), tgz, resp.GetObjectName(), resp.GetUrl()); err != nil {
		return fmt.Errorf("unable to upload version file to GCS: %s", err)
	}
	if _, err = client.WriteTGZFromURL(ctx, &protos.WriteTGZFromURLRequest{
		GomoteId:  name,
		Directory: dir,
		Url:       fmt.Sprintf("%s%s", resp.GetUrl(), resp.GetObjectName()),
	}); err != nil {
		return fmt.Errorf("unable to write tar to instance: %s", statusFromError(err))
	}
	return nil
}

func doPutTar(ctx context.Context, name, dir string, tgz io.Reader) error {
	client := gomoteServerClient(ctx)
	resp, err := client.UploadFile(ctx, &protos.UploadFileRequest{})
	if err != nil {
		return fmt.Errorf("unable to request credentials for a file upload: %s", statusFromError(err))
	}
	if err := uploadToGCS(ctx, resp.GetFields(), tgz, resp.GetObjectName(), resp.GetUrl()); err != nil {
		return fmt.Errorf("unable to upload file to GCS: %s", err)
	}
	if _, err := client.WriteTGZFromURL(ctx, &protos.WriteTGZFromURLRequest{
		GomoteId:  name,
		Directory: dir,
		Url:       fmt.Sprintf("%s%s", resp.GetUrl(), resp.GetObjectName()),
	}); err != nil {
		return fmt.Errorf("unable to write tar to instance: %s", statusFromError(err))
	}
	return nil
}

// put go1.4 in the workdir
func put14(args []string) error {
	if activeGroup != nil {
		return fmt.Errorf("command does not support groups")
	}

	fs := flag.NewFlagSet("put14", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "put14 usage: gomote put14 <buildlet-name>")
		fs.PrintDefaults()
		os.Exit(1)
	}
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
	}
	name := fs.Arg(0)
	bc, conf, err := clientAndConf(name)
	if err != nil {
		return err
	}
	u := conf.GoBootstrapURL(buildEnv)
	if u == "" {
		fmt.Printf("No GoBootstrapURL defined for %q; ignoring. (may be baked into image)\n", name)
		return nil
	}
	ctx := context.Background()
	return bc.PutTarFromURL(ctx, u, "go1.4")
}

// putBootstrap places the bootstrap version of go in the workdir
func putBootstrap(args []string) error {
	fs := flag.NewFlagSet("putbootstrap", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "putbootstrap usage: gomote putbootstrap [instance]")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Instance name is optional if a group is specified.")
		fs.PrintDefaults()
		os.Exit(1)
	}
	fs.Parse(args)

	var putSet []string
	switch fs.NArg() {
	case 0:
		if activeGroup == nil {
			fmt.Fprintln(os.Stderr, "no active group found; need an active group with only 1 argument")
			fs.Usage()
		}
		for _, inst := range activeGroup.Instances {
			putSet = append(putSet, inst)
		}
	case 1:
		putSet = []string{fs.Arg(0)}
	default:
		fmt.Fprintln(os.Stderr, "error: too many arguments")
		fs.Usage()
	}

	eg, ctx := errgroup.WithContext(context.Background())
	for _, inst := range putSet {
		inst := inst
		eg.Go(func() error {
			client := gomoteServerClient(ctx)
			resp, err := client.AddBootstrap(ctx, &protos.AddBootstrapRequest{
				GomoteId: inst,
			})
			if err != nil {
				return fmt.Errorf("unable to add bootstrap version of Go to instance: %s", statusFromError(err))
			}
			if resp.GetBootstrapGoUrl() == "" {
				fmt.Printf("No GoBootstrapURL defined for %q; ignoring. (may be baked into image)\n", inst)
			}
			return nil
		})
	}
	return eg.Wait()
}

// legacyPut single file
func legacyPut(args []string) error {
	if activeGroup != nil {
		return fmt.Errorf("command does not support groups")
	}

	fs := flag.NewFlagSet("put", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "put usage: gomote put [put-opts] <buildlet-name> <source or '-' for stdin> [destination]")
		fs.PrintDefaults()
		os.Exit(1)
	}
	modeStr := fs.String("mode", "", "Unix file mode (octal); default to source file mode")
	fs.Parse(args)
	if n := fs.NArg(); n < 2 || n > 3 {
		fs.Usage()
	}

	bc, err := remoteClient(fs.Arg(0))
	if err != nil {
		return err
	}

	var r io.Reader = os.Stdin
	var mode os.FileMode = 0666

	src := fs.Arg(1)
	if src != "-" {
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		r = f

		if *modeStr == "" {
			fi, err := f.Stat()
			if err != nil {
				return err
			}
			mode = fi.Mode()
		}
	}
	if *modeStr != "" {
		modeInt, err := strconv.ParseInt(*modeStr, 8, 64)
		if err != nil {
			return err
		}
		mode = os.FileMode(modeInt)
		if !mode.IsRegular() {
			return fmt.Errorf("bad mode: %v", mode)
		}
	}

	dest := fs.Arg(2)
	if dest == "" {
		if src == "-" {
			return errors.New("must specify destination file name when source is standard input")
		}
		dest = filepath.Base(src)
	}

	ctx := context.Background()
	return bc.Put(ctx, r, dest, mode)
}

// put single file
func put(args []string) error {
	fs := flag.NewFlagSet("put", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "put usage: gomote put [put-opts] [instance] <source or '-' for stdin> [destination]")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Instance name is optional if a group is specified.")
		fs.PrintDefaults()
		os.Exit(1)
	}
	modeStr := fs.String("mode", "", "Unix file mode (octal); default to source file mode")
	fs.Parse(args)

	if fs.NArg() == 0 {
		fs.Usage()
	}

	ctx := context.Background()
	var putSet []string
	var src, dst string
	if err := doPing(ctx, fs.Arg(0)); instanceDoesNotExist(err) {
		// When there's no active group, this is just an error.
		if activeGroup == nil {
			return fmt.Errorf("instance %q: %s", fs.Arg(0), statusFromError(err))
		}
		// When there is an active group, this just means that we're going
		// to use the group instead and assume the rest is a command.
		for _, inst := range activeGroup.Instances {
			putSet = append(putSet, inst)
		}
		src = fs.Arg(0)
		if fs.NArg() == 2 {
			dst = fs.Arg(1)
		} else if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "error: too many arguments")
			fs.Usage()
		}
	} else if err == nil {
		putSet = append(putSet, fs.Arg(0))
		if fs.NArg() == 1 {
			fmt.Fprintln(os.Stderr, "error: missing source")
			fs.Usage()
		}
		src = fs.Arg(1)
		if fs.NArg() == 3 {
			dst = fs.Arg(2)
		} else if fs.NArg() != 2 {
			fmt.Fprintln(os.Stderr, "error: too many arguments")
			fs.Usage()
		}
	} else {
		return fmt.Errorf("checking instance %q: %v", fs.Arg(0), err)
	}
	if dst == "" {
		if src == "-" {
			return errors.New("must specify destination file name when source is standard input")
		}
		dst = filepath.Base(src)
	}

	var mode os.FileMode = 0666
	if *modeStr != "" {
		modeInt, err := strconv.ParseInt(*modeStr, 8, 64)
		if err != nil {
			return err
		}
		mode = os.FileMode(modeInt)
		if !mode.IsRegular() {
			return fmt.Errorf("bad mode: %v", mode)
		}
	}

	var putFileFn func(context.Context, string) error
	if src == "-" {
		var buf bytes.Buffer
		_, err := io.Copy(&buf, os.Stdin)
		if err != nil {
			return fmt.Errorf("reading from stdin: %v", err)
		}
		sharedFileBuf := buf.Bytes()
		putFileFn = func(ctx context.Context, inst string) error {
			return doPutFile(ctx, inst, bytes.NewReader(sharedFileBuf), dst, mode)
		}
	} else {
		putFileFn = func(ctx context.Context, inst string) error {
			f, err := os.Open(src)
			if err != nil {
				return err
			}
			defer f.Close()

			if *modeStr == "" {
				fi, err := f.Stat()
				if err != nil {
					return err
				}
				mode = fi.Mode()
			}
			return doPutFile(ctx, inst, f, dst, mode)
		}
	}

	eg, ctx := errgroup.WithContext(ctx)
	for _, inst := range putSet {
		inst := inst
		eg.Go(func() error {
			return putFileFn(ctx, inst)
		})
	}
	return eg.Wait()
}

func doPutFile(ctx context.Context, inst string, r io.Reader, dst string, mode os.FileMode) error {
	client := gomoteServerClient(ctx)
	resp, err := client.UploadFile(ctx, &protos.UploadFileRequest{})
	if err != nil {
		return fmt.Errorf("unable to request credentials for a file upload: %s", statusFromError(err))
	}
	err = uploadToGCS(ctx, resp.GetFields(), r, dst, resp.GetUrl())
	if err != nil {
		return fmt.Errorf("unable to upload file to GCS: %s", err)
	}
	_, err = client.WriteFileFromURL(ctx, &protos.WriteFileFromURLRequest{
		GomoteId: inst,
		Url:      fmt.Sprintf("%s%s", resp.GetUrl(), resp.GetObjectName()),
		Filename: dst,
		Mode:     uint32(mode),
	})
	if err != nil {
		return fmt.Errorf("unable to write the file from URL: %s", statusFromError(err))
	}
	return nil
}

func uploadToGCS(ctx context.Context, fields map[string]string, file io.Reader, filename, url string) error {
	buf := new(bytes.Buffer)
	mw := multipart.NewWriter(buf)

	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			return fmt.Errorf("unable to write field: %s", err)
		}
	}
	_, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return fmt.Errorf("unable to create form file: %s", err)
	}
	// Write our own boundary to avoid buffering entire file into the multipart Writer
	bound := fmt.Sprintf("\r\n--%s--\r\n", mw.Boundary())
	req, err := http.NewRequestWithContext(ctx, "POST", url, io.NopCloser(io.MultiReader(buf, file, strings.NewReader(bound))))
	if err != nil {
		return fmt.Errorf("unable to create request: %s", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("http request failed: %s", err)
	}
	if res.StatusCode != http.StatusNoContent {
		return fmt.Errorf("http post failed: status code=%d", res.StatusCode)
	}
	return nil
}
