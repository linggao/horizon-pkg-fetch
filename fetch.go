package fetch

import (
	"encoding/json"
	"fmt"
	"github.com/golang/glog"
	"github.com/open-horizon/horizon-pkg-fetch/horizonpkg"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sync"
)

func writeFile(destinationDir string, fileName string, content []byte) (string, error) {
	destFilePath := path.Join(destinationDir, fileName)
	// this'll overwrite
	if err := ioutil.WriteFile(destFilePath, content, 0600); err != nil {
		return "", err
	}

	return destFilePath, nil
}

// side effect: stores the pkgMeta file in destinationDir
func fetchPkgMeta(client *http.Client, pkgURL string, destinationDir string) (*horizonpkg.Pkg, error) {

	glog.V(5).Infof("Fetching Pkg from %v", pkgURL)

	// fetch, hydrate
	response, err := client.Get(pkgURL)
	if err != nil {
		return nil, err
	}

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Unexpected status code in response to Horizon Pkg fetch: %v", response.StatusCode)
	}
	defer response.Body.Close()
	rawBody, err := ioutil.ReadAll(response.Body)

	var pkg horizonpkg.Pkg
	if err := json.Unmarshal(rawBody, &pkg); err != nil {
		return nil, err
	}

	fetchFilePath, err := writeFile(destinationDir, fmt.Sprintf("%v.json", pkg.ID), rawBody)
	if err != nil {
		return nil, err
	}

	glog.V(2).Infof("Wrote PkgMeta to %v", fetchFilePath)

	// TODO: dump all pkg content (both meta and parts) to debug

	return &pkg, nil
}

func precheckPkgParts(pkg *horizonpkg.Pkg) error {
	for _, part := range pkg.Parts {
		repoTag, exists := pkg.Meta.Provides.Images[part.ID]
		if !exists {
			return fmt.Errorf("Error in pkg file: Meta.Provides is expected to contain metadata about each part and it is missing info about part %v", part)
		}
		glog.V(2).Infof("Precheck of container part info %v (part id: %v) passed, will fetch", repoTag, part.ID)

	}

	return nil
}

// VerificationError extends error, indicating a problem verifying a Pkg part
type VerificationError struct {
	msg string
}

// Error returns the error message in this error
func (e VerificationError) Error() string {
	return e.msg
}

type fetchErrRecorder struct {
	Errors    map[string]error
	WriteLock sync.Mutex
}

func newFetchErrRecorder() fetchErrRecorder {
	return fetchErrRecorder{
		Errors:    make(map[string]error),
		WriteLock: sync.Mutex{},
	}
}

func fetchPkgPart(client *http.Client, partPath string, expectedBytes int64, sources []horizonpkg.PartSource) error {
	tryOpen := func(path string) (*os.File, error) {
		return os.OpenFile(partPath, os.O_RDWR|os.O_CREATE, 0600)
	}

	tryRemove := func(f *os.File, msg string) error {
		glog.Error(msg)

		f.Close()
		err := os.Remove(f.Name())
		if err != nil {
			return err
		}

		return nil
	}

	var partFile *os.File
	var openErr error
	partFile, openErr = tryOpen(partPath)

	if openErr != nil && os.IsExist(openErr) {

		info, statErr := os.Stat(partPath)
		if statErr != nil {
			err := tryRemove(partFile, fmt.Sprintf("Error getting status for file %v although it exists. Will attempt to delete it and continue", partPath))
			if err != nil {
				return err
			}

		} else if info.Size() == expectedBytes {
			glog.V(3).Infof("Part file %v exists on disk and it has the appropriate size, skipping redownload", partPath)
			return nil
		} else {
			// TODO: can try resume here if we have an HTTP server that knows how to handle it
			err := tryRemove(partFile, fmt.Sprintf("Part file %v exists on disk but it's not complete (%v bytes and should be %v bytes). Deleting it and trying again", partPath, info.Size(), expectedBytes))
			if err != nil {
				return err
			}
		}
		partFile.Close()
		partFile, openErr = tryOpen(partPath)
		if openErr != nil {
			return openErr
		}
	}

	// we are clean, try download
	for _, source := range sources {
		response, err := client.Get(source.URL)
		if err != nil || response.StatusCode != 200 {
			glog.Errorf("Failed to download part %v from %v. Response: %v. Error: %v", partPath, source, response, err)
		} else {
			defer response.Body.Close()
			bytes, err := io.Copy(partFile, response.Body)
			if err != nil {
				return fmt.Errorf("IO copy from HTTP response body failed on part: %v. Error: %v", partPath, err)
			}

			if bytes != expectedBytes {
				glog.Errorf("Error in download and copy of part %v from %v", partPath, source)

				// ignore error, give it another shot
				tryRemove(partFile, fmt.Sprintf("Error in download and copy of part %v from %v", partPath, source))

				partFile, openErr = tryOpen(partPath)
				if openErr != nil {
					return openErr
				}
				defer partFile.Close()
				continue
			} else {
				glog.V(2).Infof("Successfully wrote %v", partPath)
				return nil
			}
		}
	}

	// try fetching a part from each source, if all fail exit with error
	return fmt.Errorf("Failed to complete download of %v", partPath)
}

// all provided signatures must match keys in userKeysDir
func verifyPkgPart(userKeysDir string, partPath string, signatures []string) error {

	glog.V(5).Infof("Verifying pkg part %v with userKeysDir %v and signatures %v", partPath, userKeysDir, signatures)

	// TODO: you were here!!!

	// skip download if file name exists

	// try fetching a part from each source, if all fail *delete the bogus file* and exit with error
	return VerificationError{fmt.Sprintf("Verification of failed of part %v", partPath)}
}

func fetchAndVerify(httpClientFactory func(overrideTimeoutS *uint) *http.Client, parts horizonpkg.DockerImageParts, destinationDir string, userKeysDir string) ([]string, error) {
	fetchErrs := newFetchErrRecorder()
	var fetched []string

	addResult := func(id string, err error, partPath string) {
		fetchErrs.WriteLock.Lock()
		defer fetchErrs.WriteLock.Unlock()

		if err != nil {
			// record failures

			glog.V(6).Infof("Recording fetch error: %v with key: %v", err, id)
			fetchErrs.Errors[id] = err
		} else if partPath != "" {
			// success
			var abs string
			abs, err = filepath.Abs(partPath)
			if err != nil {
				fetched = append(fetched, abs)
			}
			fetchErrs.Errors[id] = err
		}
	}

	var group sync.WaitGroup

	for name, part := range parts {

		group.Add(1)

		// wrap up the functionality per part; (note that we avoid problematic closed-over iteration vars in the go routine)
		go func(name string, part horizonpkg.DockerImagePart) {
			defer group.Done()

			// we don't care about file extensions if they're not in the ID
			partPath := path.Join(destinationDir, name)

			glog.V(5).Infof("Dispatched goroutine to download (%v) to path: %v (part: %v)", name, partPath, part)

			glog.V(2).Infof("Fetching %v", part.ID)
			addResult(name, fetchPkgPart(httpClientFactory(nil), partPath, part.Bytes, part.Sources), partPath)

			// TODO: support retries here
			if len(fetchErrs.Errors) == 0 {
				glog.V(2).Infof("Verifying %v", part)
				addResult(name, verifyPkgPart(userKeysDir, partPath, part.Signatures), partPath)
			}

		}(name, part)
	}

	group.Wait()

	if len(fetchErrs.Errors) > 0 {
		return nil, fmt.Errorf("Error fetching parts. Errors: %v", &fetchErrs)
	}

	return fetched, nil
}

// PkgFetch ...
//     pkgURL is the URL of the pkg file containing the image content
func PkgFetch(httpClientFactory func(overrideTimeoutS *uint) *http.Client, pkgURL *url.URL, destinationDir string, userKeysDir string) ([]string, error) {
	client := httpClientFactory(nil)

	pkg, err := fetchPkgMeta(client, pkgURL.String(), destinationDir)
	if err != nil {
		return nil, err
	}

	// we do this separately so we have a greater chance of the async fetches succeeding before we start them all
	if err := precheckPkgParts(pkg); err != nil {
		return nil, err
	}

	// make pkg subdirectory in destination directory
	pkgDir := path.Join(destinationDir, pkg.ID)
	if err := os.MkdirAll(pkgDir, 0700); err != nil {
		return nil, err
	}

	var fetched []string
	fetched, err = fetchAndVerify(httpClientFactory, pkg.Parts, destinationDir, userKeysDir)
	if err != nil {
		return nil, err
	}

	// TODO: expand to return the .fetch file; also shortcut some fetch operations if it exists
	// for now we just return the old-style image files slice

	return fetched, nil
}
