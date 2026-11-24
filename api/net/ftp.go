package net

import (
	"bufio"
	"context"
	"io"
	"net/textproto"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/alfg/openencoder/api/types"
	"github.com/jlaffaye/ftp"
)

const (
	// ErrorFileExists error return from FTP client.
	ErrorFileExists = "Can't create directory: File exists"
)

// FTP connection details.
type FTP struct {
	Addr     string
	Username string
	Password string
	Timeout  time.Duration
}

// NewFTP creates a new FTP instance.
func NewFTP(addr string, username string, password string) *FTP {
	return &FTP{
		Addr:     addr,
		Username: username,
		Password: password,
		Timeout:  5,
	}
}

// dial opens an FTP connection bound to ctx so a cancelled job aborts promptly.
func (f *FTP) dial(ctx context.Context) (*ftp.ServerConn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return ftp.Dial(f.Addr,
		ftp.DialWithTimeout(f.Timeout*time.Second),
		ftp.DialWithContext(ctx))
}

// Download download a file from an FTP connection.
func (f *FTP) Download(ctx context.Context, job types.Job) error {
	if ctx == nil {
		ctx = context.Background()
	}
	log.Info("downloading from FTP: ", job.Source)

	// Create FTP connection.
	c, err := f.dial(ctx)
	if err != nil {
		log.Error(err)
		return err
	}

	// Login.
	if err = c.Login(f.Username, f.Password); err != nil {
		log.Error(err)
		c.Quit()
		return err
	}

	resp, err := c.Retr(job.Source)
	if err != nil {
		log.Error(err)
		c.Quit()
		return err
	}
	defer resp.Close()

	outputFile, err := os.OpenFile(job.LocalSource, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		resp.Close()
		c.Quit()
		return err
	}
	defer outputFile.Close()

	reader := bufio.NewReader(resp)
	p := make([]byte, 1024*4)

	for {
		if err := ctx.Err(); err != nil {
			c.Quit()
			return err
		}
		n, readErr := reader.Read(p)
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			c.Quit()
			return readErr
		}
		if _, writeErr := outputFile.Write(p[:n]); writeErr != nil {
			c.Quit()
			return writeErr
		}
	}

	// Quit connection.
	if err := c.Quit(); err != nil {
		log.Error(err)
		return err
	}
	return ctx.Err()
}

// Upload uploads a file to FTP.
func (f *FTP) Upload(ctx context.Context, job types.Job) error {
	if ctx == nil {
		ctx = context.Background()
	}
	log.Info("uploading files to FTP: ", job.Destination)
	defer log.Info("upload complete")

	// Get list of files in output dir.
	filelist, err := listFiles(path.Dir(job.LocalSource) + "/dst")
	if err != nil {
		return err
	}

	return f.uploadDir(ctx, filelist, job)
}

func (f *FTP) uploadDir(ctx context.Context, filelist []string, job types.Job) error {
	for _, file := range filelist {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := f.uploadFile(ctx, file, job); err != nil {
			return err
		}
	}
	return nil
}

// uploadFile uploads a file from an FTP connection.
func (f *FTP) uploadFile(ctx context.Context, filePath string, job types.Job) error {
	// Create FTP connection.
	c, err := f.dial(ctx)
	if err != nil {
		return err
	}

	// Login.
	if err = c.Login(f.Username, f.Password); err != nil {
		c.Quit()
		return err
	}

	file, err := os.Open(filePath)
	if err != nil {
		c.Quit()
		return err
	}
	defer file.Close()
	reader := bufio.NewReader(file)

	// Set destination path.
	parsedURL, _ := url.Parse(job.Destination)
	key := parsedURL.Path + filepath.Base(filePath)

	// Create directory. A pre-existing directory is fine.
	if err = c.MakeDir(parsedURL.Path); err != nil {
		if protoErr, ok := err.(*textproto.Error); !ok || protoErr.Msg != ErrorFileExists {
			log.Error(err)
			c.Quit()
			return err
		}
	}

	if err = c.Stor(key, reader); err != nil {
		log.Error(err)
		c.Quit()
		return err
	}
	if err := c.Quit(); err != nil {
		log.Error(err)
		return err
	}
	return ctx.Err()
}

// ListFiles lists FTP files for a given prefix.
func (f *FTP) ListFiles(prefix string) ([]*ftp.Entry, error) {
	c, err := ftp.Dial(f.Addr, ftp.DialWithTimeout(f.Timeout*time.Second))
	if err != nil {
		log.Error(err)
		return nil, err
	}

	if err = c.Login(f.Username, f.Password); err != nil {
		c.Quit()
		return nil, err
	}

	entries, err := c.List(prefix)

	if err := c.Quit(); err != nil {
		log.Error(err)
		return nil, err
	}
	return entries, err
}
