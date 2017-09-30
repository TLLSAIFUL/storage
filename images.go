package storage

import (
	"encoding/json"
	"io/ioutil"
	"os"
	"path/filepath"
	"time"

	"github.com/containers/storage/pkg/ioutils"
	"github.com/containers/storage/pkg/stringid"
	"github.com/containers/storage/pkg/truncindex"
	"github.com/pkg/errors"
)

// An Image is a reference to a layer and an associated metadata string.
type Image struct {
	// ID is either one which was specified at create-time, or a random
	// value which was generated by the library.
	ID string `json:"id"`

	// Names is an optional set of user-defined convenience values.  The
	// image can be referred to by its ID or any of its names.  Names are
	// unique among images.
	Names []string `json:"names,omitempty"`

	// TopLayer is the ID of the topmost layer of the image itself, if the
	// image contains one or more layers.  Multiple images can refer to the
	// same top layer.
	TopLayer string `json:"layer"`

	// Metadata is data we keep for the convenience of the caller.  It is not
	// expected to be large, since it is kept in memory.
	Metadata string `json:"metadata,omitempty"`

	// BigDataNames is a list of names of data items that we keep for the
	// convenience of the caller.  They can be large, and are only in
	// memory when being read from or written to disk.
	BigDataNames []string `json:"big-data-names,omitempty"`

	// BigDataSizes maps the names in BigDataNames to the sizes of the data
	// that has been stored, if they're known.
	BigDataSizes map[string]int64 `json:"big-data-sizes,omitempty"`

	// Created is the datestamp for when this image was created.  Older
	// versions of the library did not track this information, so callers
	// will likely want to use the IsZero() method to verify that a value
	// is set before using it.
	Created time.Time `json:"created,omitempty"`

	Flags map[string]interface{} `json:"flags,omitempty"`
}

// ROImageStore provides bookkeeping for information about Images.
type ROImageStore interface {
	ROFileBasedStore
	ROMetadataStore
	ROBigDataStore

	// Exists checks if there is an image with the given ID or name.
	Exists(id string) bool

	// Get retrieves information about an image given an ID or name.
	Get(id string) (*Image, error)

	// Lookup attempts to translate a name to an ID.  Most methods do this
	// implicitly.
	Lookup(name string) (string, error)

	// Images returns a slice enumerating the known images.
	Images() ([]Image, error)
}

// ImageStore provides bookkeeping for information about Images.
type ImageStore interface {
	ROImageStore
	RWFileBasedStore
	RWMetadataStore
	RWBigDataStore
	FlaggableStore

	// Create creates an image that has a specified ID (or a random one) and
	// optional names, using the specified layer as its topmost (hopefully
	// read-only) layer.  That layer can be referenced by multiple images.
	Create(id string, names []string, layer, metadata string, created time.Time) (*Image, error)

	// SetNames replaces the list of names associated with an image with the
	// supplied values.
	SetNames(id string, names []string) error

	// Delete removes the record of the image.
	Delete(id string) error

	// Wipe removes records of all images.
	Wipe() error
}

type imageStore struct {
	lockfile Locker
	dir      string
	images   []*Image
	idindex  *truncindex.TruncIndex
	byid     map[string]*Image
	byname   map[string]*Image
}

func (r *imageStore) Images() ([]Image, error) {
	images := make([]Image, len(r.images))
	for i := range r.images {
		images[i] = *(r.images[i])
	}
	return images, nil
}

func (r *imageStore) imagespath() string {
	return filepath.Join(r.dir, "images.json")
}

func (r *imageStore) datadir(id string) string {
	return filepath.Join(r.dir, id)
}

func (r *imageStore) datapath(id, key string) string {
	return filepath.Join(r.datadir(id), makeBigDataBaseName(key))
}

func (r *imageStore) Load() error {
	shouldSave := false
	rpath := r.imagespath()
	data, err := ioutil.ReadFile(rpath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	images := []*Image{}
	idlist := []string{}
	ids := make(map[string]*Image)
	names := make(map[string]*Image)
	if err = json.Unmarshal(data, &images); len(data) == 0 || err == nil {
		idlist = make([]string, 0, len(images))
		for n, image := range images {
			ids[image.ID] = images[n]
			idlist = append(idlist, image.ID)
			for _, name := range image.Names {
				if conflict, ok := names[name]; ok {
					r.removeName(conflict, name)
					shouldSave = true
				}
				names[name] = images[n]
			}
		}
	}
	if shouldSave && !r.IsReadWrite() {
		return ErrDuplicateImageNames
	}
	r.images = images
	r.idindex = truncindex.NewTruncIndex(idlist)
	r.byid = ids
	r.byname = names
	if shouldSave {
		return r.Save()
	}
	return nil
}

func (r *imageStore) Save() error {
	if !r.IsReadWrite() {
		return errors.Wrapf(ErrStoreIsReadOnly, "not allowed to modify the image store at %q", r.imagespath())
	}
	rpath := r.imagespath()
	if err := os.MkdirAll(filepath.Dir(rpath), 0700); err != nil {
		return err
	}
	jdata, err := json.Marshal(&r.images)
	if err != nil {
		return err
	}
	defer r.Touch()
	return ioutils.AtomicWriteFile(rpath, jdata, 0600)
}

func newImageStore(dir string) (ImageStore, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	lockfile, err := GetLockfile(filepath.Join(dir, "images.lock"))
	if err != nil {
		return nil, err
	}
	lockfile.Lock()
	defer lockfile.Unlock()
	istore := imageStore{
		lockfile: lockfile,
		dir:      dir,
		images:   []*Image{},
		byid:     make(map[string]*Image),
		byname:   make(map[string]*Image),
	}
	if err := istore.Load(); err != nil {
		return nil, err
	}
	return &istore, nil
}

func newROImageStore(dir string) (ROImageStore, error) {
	lockfile, err := GetROLockfile(filepath.Join(dir, "images.lock"))
	if err != nil {
		return nil, err
	}
	lockfile.Lock()
	defer lockfile.Unlock()
	istore := imageStore{
		lockfile: lockfile,
		dir:      dir,
		images:   []*Image{},
		byid:     make(map[string]*Image),
		byname:   make(map[string]*Image),
	}
	if err := istore.Load(); err != nil {
		return nil, err
	}
	return &istore, nil
}

func (r *imageStore) lookup(id string) (*Image, bool) {
	if image, ok := r.byid[id]; ok {
		return image, ok
	} else if image, ok := r.byname[id]; ok {
		return image, ok
	} else if longid, err := r.idindex.Get(id); err == nil {
		image, ok := r.byid[longid]
		return image, ok
	}
	return nil, false
}

func (r *imageStore) ClearFlag(id string, flag string) error {
	if !r.IsReadWrite() {
		return errors.Wrapf(ErrStoreIsReadOnly, "not allowed to clear flags on images at %q", r.imagespath())
	}
	image, ok := r.lookup(id)
	if !ok {
		return ErrImageUnknown
	}
	delete(image.Flags, flag)
	return r.Save()
}

func (r *imageStore) SetFlag(id string, flag string, value interface{}) error {
	if !r.IsReadWrite() {
		return errors.Wrapf(ErrStoreIsReadOnly, "not allowed to set flags on images at %q", r.imagespath())
	}
	image, ok := r.lookup(id)
	if !ok {
		return ErrImageUnknown
	}
	if image.Flags == nil {
		image.Flags = make(map[string]interface{})
	}
	image.Flags[flag] = value
	return r.Save()
}

func (r *imageStore) Create(id string, names []string, layer, metadata string, created time.Time) (image *Image, err error) {
	if !r.IsReadWrite() {
		return nil, errors.Wrapf(ErrStoreIsReadOnly, "not allowed to create new images at %q", r.imagespath())
	}
	if id == "" {
		id = stringid.GenerateRandomID()
		_, idInUse := r.byid[id]
		for idInUse {
			id = stringid.GenerateRandomID()
			_, idInUse = r.byid[id]
		}
	}
	if _, idInUse := r.byid[id]; idInUse {
		return nil, ErrDuplicateID
	}
	names = dedupeNames(names)
	for _, name := range names {
		if _, nameInUse := r.byname[name]; nameInUse {
			return nil, ErrDuplicateName
		}
	}
	if created.IsZero() {
		created = time.Now().UTC()
	}
	if err == nil {
		image = &Image{
			ID:           id,
			Names:        names,
			TopLayer:     layer,
			Metadata:     metadata,
			BigDataNames: []string{},
			BigDataSizes: make(map[string]int64),
			Created:      created,
			Flags:        make(map[string]interface{}),
		}
		r.images = append(r.images, image)
		r.idindex.Add(id)
		r.byid[id] = image
		for _, name := range names {
			r.byname[name] = image
		}
		err = r.Save()
	}
	return image, err
}

func (r *imageStore) Metadata(id string) (string, error) {
	if image, ok := r.lookup(id); ok {
		return image.Metadata, nil
	}
	return "", ErrImageUnknown
}

func (r *imageStore) SetMetadata(id, metadata string) error {
	if !r.IsReadWrite() {
		return errors.Wrapf(ErrStoreIsReadOnly, "not allowed to modify image metadata at %q", r.imagespath())
	}
	if image, ok := r.lookup(id); ok {
		image.Metadata = metadata
		return r.Save()
	}
	return ErrImageUnknown
}

func (r *imageStore) removeName(image *Image, name string) {
	image.Names = stringSliceWithoutValue(image.Names, name)
}

func (r *imageStore) SetNames(id string, names []string) error {
	if !r.IsReadWrite() {
		return errors.Wrapf(ErrStoreIsReadOnly, "not allowed to change image name assignments at %q", r.imagespath())
	}
	names = dedupeNames(names)
	if image, ok := r.lookup(id); ok {
		for _, name := range image.Names {
			delete(r.byname, name)
		}
		for _, name := range names {
			if otherImage, ok := r.byname[name]; ok {
				r.removeName(otherImage, name)
			}
			r.byname[name] = image
		}
		image.Names = names
		return r.Save()
	}
	return ErrImageUnknown
}

func (r *imageStore) Delete(id string) error {
	if !r.IsReadWrite() {
		return errors.Wrapf(ErrStoreIsReadOnly, "not allowed to delete images at %q", r.imagespath())
	}
	image, ok := r.lookup(id)
	if !ok {
		return ErrImageUnknown
	}
	id = image.ID
	toDeleteIndex := -1
	for i, candidate := range r.images {
		if candidate.ID == id {
			toDeleteIndex = i
		}
	}
	delete(r.byid, id)
	r.idindex.Delete(id)
	for _, name := range image.Names {
		delete(r.byname, name)
	}
	if toDeleteIndex != -1 {
		// delete the image at toDeleteIndex
		if toDeleteIndex == len(r.images)-1 {
			r.images = r.images[:len(r.images)-1]
		} else {
			r.images = append(r.images[:toDeleteIndex], r.images[toDeleteIndex+1:]...)
		}
	}
	if err := r.Save(); err != nil {
		return err
	}
	if err := os.RemoveAll(r.datadir(id)); err != nil {
		return err
	}
	return nil
}

func (r *imageStore) Get(id string) (*Image, error) {
	if image, ok := r.lookup(id); ok {
		return image, nil
	}
	return nil, ErrImageUnknown
}

func (r *imageStore) Lookup(name string) (id string, err error) {
	if image, ok := r.lookup(name); ok {
		return image.ID, nil
	}
	return "", ErrImageUnknown
}

func (r *imageStore) Exists(id string) bool {
	_, ok := r.lookup(id)
	return ok
}

func (r *imageStore) BigData(id, key string) ([]byte, error) {
	if key == "" {
		return nil, errors.Wrapf(ErrInvalidBigDataName, "data name %q can not be used as a filename", key)
	}
	image, ok := r.lookup(id)
	if !ok {
		return nil, ErrImageUnknown
	}
	return ioutil.ReadFile(r.datapath(image.ID, key))
}

func (r *imageStore) BigDataSize(id, key string) (int64, error) {
	if key == "" {
		return -1, errors.Wrapf(ErrInvalidBigDataName, "data name %q can not be used as a filename", key)
	}
	image, ok := r.lookup(id)
	if !ok {
		return -1, ErrImageUnknown
	}
	if image.BigDataSizes == nil {
		image.BigDataSizes = make(map[string]int64)
	}
	if size, ok := image.BigDataSizes[key]; ok {
		return size, nil
	}
	return -1, ErrSizeUnknown
}

func (r *imageStore) BigDataNames(id string) ([]string, error) {
	image, ok := r.lookup(id)
	if !ok {
		return nil, ErrImageUnknown
	}
	return image.BigDataNames, nil
}

func (r *imageStore) SetBigData(id, key string, data []byte) error {
	if !r.IsReadWrite() {
		return errors.Wrapf(ErrStoreIsReadOnly, "not allowed to save data items associated with images at %q", r.imagespath())
	}
	image, ok := r.lookup(id)
	if !ok {
		return ErrImageUnknown
	}
	if err := os.MkdirAll(r.datadir(image.ID), 0700); err != nil {
		return err
	}
	err := ioutils.AtomicWriteFile(r.datapath(image.ID, key), data, 0600)
	if err == nil {
		add := true
		save := false
		if image.BigDataSizes == nil {
			image.BigDataSizes = make(map[string]int64)
		}
		oldSize, sizeOk := image.BigDataSizes[key]
		image.BigDataSizes[key] = int64(len(data))
		if !sizeOk || oldSize != image.BigDataSizes[key] {
			save = true
		}
		for _, name := range image.BigDataNames {
			if name == key {
				add = false
				break
			}
		}
		if add {
			image.BigDataNames = append(image.BigDataNames, key)
			save = true
		}
		if save {
			err = r.Save()
		}
	}
	return err
}

func (r *imageStore) Wipe() error {
	if !r.IsReadWrite() {
		return errors.Wrapf(ErrStoreIsReadOnly, "not allowed to delete images at %q", r.imagespath())
	}
	ids := make([]string, 0, len(r.byid))
	for id := range r.byid {
		ids = append(ids, id)
	}
	for _, id := range ids {
		if err := r.Delete(id); err != nil {
			return err
		}
	}
	return nil
}

func (r *imageStore) Lock() {
	r.lockfile.Lock()
}

func (r *imageStore) Unlock() {
	r.lockfile.Unlock()
}

func (r *imageStore) Touch() error {
	return r.lockfile.Touch()
}

func (r *imageStore) Modified() (bool, error) {
	return r.lockfile.Modified()
}

func (r *imageStore) IsReadWrite() bool {
	return r.lockfile.IsReadWrite()
}

func (r *imageStore) TouchedSince(when time.Time) bool {
	return r.lockfile.TouchedSince(when)
}
