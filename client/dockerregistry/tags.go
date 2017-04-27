package dockerregistry

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"code.uber.internal/go-common.git/x/log"
	"code.uber.internal/infra/kraken/client/store"
	"code.uber.internal/infra/kraken/client/torrentclient"
	"code.uber.internal/infra/kraken/configuration"

	"github.com/docker/distribution"
	"github.com/docker/distribution/manifest/schema2"
	"github.com/docker/distribution/uuid"
	"github.com/uber-common/bark"
)

// Tags is an interface
type Tags interface {
	ListTags(repo string) ([]string, error)
	ListRepos() ([]string, error)
	DeleteTag(repo, tag string) error
	GetTag(repo, tag string) (io.ReadCloser, error)
	CreateTag(repo, tag, manifest string) ([]byte, error)
	DeleteExpiredTags(n int, expireTime time.Time) error
}

// DockerTags handles tag lookups
// a tag is a file with tag_path = <tag_dir>/<repo>/<tag>
// content of the file is sha1(<tag_path>), which is the name of a (torrent) file in cache_dir
// torrent file <cache_dir>/<sha1(<tag_path>)> is a link between tag and manifest
// the content of it is the manifest digest of the tag
type DockerTags struct {
	sync.RWMutex

	config *configuration.Config
	store  *store.LocalFileStore
	client *torrentclient.Client
}

// Tag stores information about one tag.
type Tag struct {
	repo    string
	tagName string
	modTime time.Time
}

// TagSlice is used for sorting tags
type TagSlice []Tag

func (s TagSlice) Less(i, j int) bool { return s[i].modTime.Before(s[j].modTime) }
func (s TagSlice) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }
func (s TagSlice) Len() int           { return len(s) }

// NewDockerTags returns new DockerTags
func NewDockerTags(c *configuration.Config, s *store.LocalFileStore, cl *torrentclient.Client) (Tags, error) {
	err := os.MkdirAll(c.TagDir, 0755)
	if err != nil {
		return nil, err
	}
	return &DockerTags{
		config: c,
		store:  s,
		client: cl,
	}, nil
}

// ListTags lists tags under given repo
func (t *DockerTags) ListTags(repo string) ([]string, error) {
	t.RLock()
	defer t.RUnlock()

	return t.listTags(repo)
}

// ListRepos lists repos under tag directory
// this function can be expensive if there are too many repos
func (t *DockerTags) ListRepos() ([]string, error) {
	t.RLock()
	defer t.RUnlock()

	return t.listReposFromRoot(t.getRepositoriesPath())
}

// DeleteTag deletes a tag given repo and tag
func (t *DockerTags) DeleteTag(repo, tag string) error {
	if !t.config.TagDeletion.Enable {
		return fmt.Errorf("Tag Deletion not enabled")
	}

	t.Lock()
	defer t.Unlock()

	c := make(chan byte, 1)
	var tags []string
	var listErr error
	// list tags nonblocking
	go func() {
		tags, listErr = t.listTags(repo)
		c <- 'c'
	}()

	tagSha := t.getTagHash(repo, tag)
	manifest, err := t.getManifest(repo, tag)
	if err != nil {
		return err
	}

	layers, err := t.getAllLayers(manifest)
	if err != nil {
		return err
	}

	<-c
	if listErr != nil {
		log.Errorf("Error listing tags in repo %s:%s. Err: %s", repo, tag, err.Error())
	} else {
		// remove repo along with the tag
		// if this is the last tag in the repo
		if len(tags) == 1 && tags[0] == tag {
			err = os.RemoveAll(t.getRepoPath(repo))
		} else {
			// delete tag file
			err = os.Remove(t.getTagPath(repo, tag))
		}
		if err != nil {
			return err
		}
	}

	// delete tag torrent
	err = t.store.MoveCacheFileToTrash(string(tagSha[:]))
	if err != nil {
		log.Errorf("Error deleting tag torrent for %s:%s. Err: %s", repo, tag, err.Error())
	}

	for _, layer := range layers {
		_, err := t.store.DecrementCacheFileRefCount(layer)
		if err != nil {
			// TODO (@evelynl): if decrement ref count fails, we might have some garbage layers that are never deleted
			// one possilbe solution is that we can add a reconciliation routine to re-calcalate ref count for all layers
			// another is that we use sqlite
			log.Errorf("Error decrement ref count for layer %s from %s:%s. Err: %s", layer, repo, tag, err.Error())
		}
	}
	return nil
}

// GetTag returns a reader of tag content
func (t *DockerTags) GetTag(repo, tag string) (io.ReadCloser, error) {
	return t.getOrDownloadTaglink(repo, tag)
}

// createTag creates a new tag file given repo and tag
// returns tag file sha1
func (t *DockerTags) createTag(repo, tag string, layers []string) error {
	t.Lock()
	defer t.Unlock()

	tagFp := t.getTagPath(repo, tag)

	// If tag already exists, return file exists error
	_, err := os.Stat(tagFp)
	if err == nil {
		return os.ErrExist
	}
	if !os.IsNotExist(err) {
		return err
	}

	// Create tag file
	err = os.MkdirAll(path.Dir(tagFp), 0755)
	if err != nil {
		return err
	}

	// TODO: handle the case if file already exists.
	tagSha := t.getTagHash(repo, tag)
	err = ioutil.WriteFile(tagFp, tagSha, 0755)
	if err != nil {
		return err
	}

	for _, layer := range layers {
		// TODO (@evelynl): if increment ref count fails and the caller retries, we might have some garbage layers that are never deleted
		// one possilbe solution is that we can add a reconciliation routine to re-calcalate ref count for all layers
		// another is that we use sqlite
		_, err := t.store.IncrementCacheFileRefCount(layer)
		if err != nil {
			return err
		}
	}
	return nil
}

// getOrDownloadTaglink gets a tag torrent reader or download one
func (t *DockerTags) getOrDownloadTaglink(repo, tag string) (io.ReadCloser, error) {
	tagSha := t.getTagHash(repo, tag)

	// Try get file
	var reader io.ReadCloser
	var err error
	reader, err = t.store.GetCacheFileReader(string(tagSha[:]))
	// TODO (@evelynl): check for file not found error?
	if err != nil {
		err = t.client.DownloadByName(string(tagSha[:]))
		if err != nil {
			return nil, err
		}

		reader, err = t.store.GetCacheFileReader(string(tagSha[:]))
		if err != nil {
			return nil, err
		}
	}

	// start downloading layers in advance
	go t.getOrDownloadAllLayersAndCreateTag(repo, tag)

	return reader, nil
}

// getOrDownloadAllLayersAndCreateTag downloads all data for a tag
func (t *DockerTags) getOrDownloadAllLayersAndCreateTag(repo, tag string) error {
	tagSha := t.getTagHash(repo, tag)

	// TODO (@evelynl): after moving manifest to tracker, we do not need tag and manifest torrents
	// we should get manifest from the tracker using tag and only download layers
	// but for now, download manifest first
	reader, err := t.store.GetCacheFileReader(string(tagSha[:]))
	if err != nil {
		log.Errorf("Error getting tag for %s:%s", repo, tag)
		return err
	}

	manifestDigest, err := ioutil.ReadAll(reader)
	if err != nil {
		log.Errorf("Error getting manifest for %s:%s", repo, tag)
		return err
	}

	_, err = t.store.GetCacheFileStat(string(manifestDigest[:]))
	if err != nil && os.IsNotExist(err) {
		err = t.client.DownloadByName(string(manifestDigest[:]))
	}
	if err != nil {
		log.Errorf("Error downloading manifest for %s:%s", repo, tag)
		return err
	}

	layers, err := t.getAllLayers(string(manifestDigest[:]))
	if err != nil {
		log.Errorf("Error getting layers from manifest %s for %s:%s", manifestDigest, repo, tag)
		return err
	}

	numLayers := len(layers)
	wg := &sync.WaitGroup{}
	wg.Add(numLayers)
	// errors is a channel to collect errors
	errors := make(chan error, numLayers)

	// for every layer, download if it is already
	for _, layer := range layers {
		go func(l string) {
			defer wg.Done()
			var err error
			_, err = t.store.GetCacheFileStat(l)
			if err != nil && os.IsNotExist(err) {
				err = t.client.DownloadByName(l)
			}

			if err != nil {
				log.Errorf("Error getting layer %s from manifest %s for %s:%s", l, manifestDigest, repo, tag)
				errors <- err
			}
		}(layer)
	}

	wg.Wait()
	select {
	// if there is any error downloading layers, we return without incrementing ref count nor creating tag
	case err = <-errors:
		return err
	default:
	}

	return t.createTag(repo, tag, layers)
}

// getAllLayers returns all layers referenced by the manifest, including the manifest itself.
func (t *DockerTags) getAllLayers(manifestDigest string) ([]string, error) {
	reader, err := t.store.GetCacheFileReader(manifestDigest)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	body, err := ioutil.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	manifest, _, err := distribution.UnmarshalManifest(schema2.MediaTypeManifest, body)
	if err != nil {
		return nil, err
	}
	deserializedManifest, ok := manifest.(*schema2.DeserializedManifest)
	if !ok {
		return nil, fmt.Errorf("Unable to deserialize manifest")
	}
	version := deserializedManifest.Manifest.Versioned.SchemaVersion
	if version != 2 {
		return nil, fmt.Errorf("Unsupported manifest version: %d", version)
	}

	layers := []string{manifestDigest}

	switch manifest.(type) {
	case *schema2.DeserializedManifest:
		// Inc ref count for config and data layers.
		descriptors := manifest.References()
		for _, descriptor := range descriptors {
			if descriptor.Digest == "" {
				return nil, fmt.Errorf("Unsupported layer format in manifest")
			}

			layers = append(layers, descriptor.Digest.Hex())
		}
	default:
		return nil, fmt.Errorf("Unsupported manifest format")
	}
	return layers, nil
}

// CreateTag creates a new tag given repo, tag and manifest and a new tag torrent for manifest referencing
// returns tag file sha1
func (t *DockerTags) CreateTag(repo, tag, manifest string) ([]byte, error) {
	// Create tag torrent in upload directory
	tagSha := t.getTagHash(repo, tag)
	randFileName := string(tagSha[:]) + "." + uuid.Generate().String()
	_, err := t.store.CreateUploadFile(randFileName, int64(len(manifest)))
	if err != nil {
		return nil, err
	}

	writer, err := t.store.GetUploadFileReadWriter(randFileName)
	if err != nil {
		return nil, err
	}

	// Write manifest digest to tag torrent
	_, err = writer.Write([]byte(manifest))
	if err != nil {
		writer.Close()
		return nil, err
	}
	writer.Close()

	// Move tag torrent to cache
	err = t.store.MoveUploadFileToCache(randFileName, string(tagSha[:]))
	if err == nil {
		// Create torrent
		fp, err := t.store.GetCacheFilePath(string(tagSha[:]))
		if err != nil {
			return nil, err
		}

		err = t.client.CreateTorrentFromFile(string(tagSha[:]), fp)
		if err != nil {
			return nil, err
		}
	} else if os.IsExist(err) {
		// Someone is pushing an existing tag, which is not allowed.
		// TODO: client would try to push v1 manifest after this failure, and receive 500 response due
		// to v1 manifest parsing error, which might cause confusion.
		// TODO: cleanup upload file
		return nil, err
	} else {
		return nil, err
	}

	// Inc ref for all layers and the manifest
	layers, err := t.getAllLayers(manifest)
	if err != nil {
		return nil, err
	}

	// Create tag file and increment ref count
	err = t.createTag(repo, tag, layers)
	if err != nil {
		return nil, err
	}

	log.WithFields(bark.Fields{
		"repo":     repo,
		"tag":      tag,
		"tagsha":   string(tagSha[:]),
		"manifest": manifest,
	}).Info("Successfully created tag")

	return tagSha[:], nil
}

func (t *DockerTags) listTags(repo string) ([]string, error) {
	tagInfos, err := ioutil.ReadDir(t.getRepoPath(repo))
	if err != nil {
		return nil, err
	}

	var tags []string
	for _, tagInfo := range tagInfos {
		tags = append(tags, tagInfo.Name())
	}
	return tags, nil
}

func (t *DockerTags) listReposFromRoot(root string) ([]string, error) {
	rootStat, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !rootStat.IsDir() {
		return nil, fmt.Errorf("Failed to list repos. %s is a directory", root)
	}

	repoInfos, err := ioutil.ReadDir(root)
	if err != nil {
		return nil, err
	}

	var repos []string
	foundRepo := false
	for _, repoInfo := range repoInfos {
		if repoInfo.IsDir() {
			foundRepo = true
			var subrepos []string
			var err error
			subrepos, err = t.listReposFromRoot(path.Join(root, repoInfo.Name()))
			if err != nil {
				continue
			}
			repos = append(repos, subrepos...)
		}
	}
	if foundRepo {
		return repos, nil
	}

	// All files in root are tags, return itself
	rootRepo := strings.TrimPrefix(root, t.getRepositoriesPath())
	rootRepo = strings.TrimLeft(rootRepo, "/")
	return []string{rootRepo}, nil
}

// getManifest returns manifest digest given repo and tag
func (t *DockerTags) getManifest(repo, tag string) (string, error) {
	tagSha := t.getTagHash(repo, tag)

	linkReader, err := t.store.GetCacheFileReader(string(tagSha[:]))
	if err != nil {
		log.Infof("%s", err.Error())
		return "", err
	}
	defer linkReader.Close()

	link, err := ioutil.ReadAll(linkReader)
	if err != nil {
		return "", err
	}

	return string(link[:]), nil
}

// getTaghash returns the hash of the tag reference given repo and tag
func (t *DockerTags) getTagHash(repo, tag string) []byte {
	tagFp := path.Join(repo, tag)
	rawtagSha := sha1.Sum([]byte(tagFp))
	return []byte(hex.EncodeToString(rawtagSha[:]))
}

func (t *DockerTags) getRepoPath(repo string) string {
	return path.Join(t.config.TagDir, repo)
}

func (t *DockerTags) getTagPath(repo string, tag string) string {
	return path.Join(t.config.TagDir, repo, tag)
}

func (t *DockerTags) getRepositoriesPath() string {
	return t.config.TagDir
}

// listExpiredTags lists expired tags under given repo.
// They are not in the latest n tags and reached expireTime.
func (t *DockerTags) listExpiredTags(repo string, n int, expireTime time.Time) ([]string, error) {
	t.RLock()
	defer t.RUnlock()

	tagInfos, err := ioutil.ReadDir(t.getRepoPath(repo))
	if err != nil {
		return nil, err
	}

	// Sort tags by creation time
	s := make(TagSlice, 0)
	for _, tagInfo := range tagInfos {
		tag := Tag{
			repo:    repo,
			tagName: tagInfo.Name(),
			modTime: tagInfo.ModTime(),
		}
		s = append(s, tag)
	}
	sort.Sort(s)

	var tags []string
	for i := 0; i < len(s)-n; i++ {
		if s[i].modTime.After(expireTime) {
			break
		}
		tags = append(tags, s[i].tagName)
	}

	return tags, nil
}

// DeleteExpiredTags deletes tags that are older than expireTime and not in the n latest.
func (t *DockerTags) DeleteExpiredTags(n int, expireTime time.Time) error {
	repos, err := t.ListRepos()
	if err != nil {
		return err
	}
	for _, repo := range repos {
		tags, err := t.listExpiredTags(repo, n, expireTime)
		if err != nil {
			return err
		}
		for _, tag := range tags {
			log.Infof("Deleting tag %s", tag)
			t.DeleteTag(repo, tag)
		}
	}

	return nil
}