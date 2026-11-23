package db

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/pkg/errors"
	"github.com/sfluor/musig/internal/pkg/model"
	"go.etcd.io/bbolt"
)

var _ Database = &BoltDB{}

// BoltDB implements the Database interface using a bolt database
type BoltDB struct {
	*bbolt.DB
	fingerprintBucket []byte
	songBucket        []byte
}

// NewBoltDB returns a new bolt database
func NewBoltDB(path string) (*BoltDB, error) {
	db, err := bbolt.Open(path, 0666, nil)
	if err != nil {
		return nil, errors.Wrapf(err, "error creating bolt db with path:%s", path)
	}

	boltDB := &BoltDB{
		DB:                db,
		fingerprintBucket: []byte("fingerprint"),
		songBucket:        []byte("song"),
	}

	// Create buckets
	err = db.Update(func(tx *bbolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(boltDB.fingerprintBucket); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(boltDB.songBucket); err != nil {
			return err
		}
		return nil
	})

	return boltDB, errors.Wrap(err, "error creating buckets")
}

// Get retrieves the provided keys' values from the bolt file
func (db *BoltDB) Get(keys []model.EncodedKey) (map[model.EncodedKey][]model.TableValue, error) {
	res := make(map[model.EncodedKey][]model.TableValue, len(keys))

	err := db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(db.fingerprintBucket)

		for _, k := range keys {
			raw := b.Get(k.Bytes())
			if len(raw) == 0 {
				continue
			}
			val, err := model.ValuesFromBytes(b.Get(k.Bytes()))
			if err != nil {
				return errors.Wrapf(err, "wrong record stored: %v", raw)
			}

			res[k] = val
		}

		return nil
	})

	return res, errors.Wrap(err, "an error occurred when reading from bolt")
}

// Set stores the list of (key, value) into the bolt file.
// Writes are idempotent: a (key, value) pair that is already stored for the
// same song is skipped instead of being appended again. Values belonging to
// other songs sharing the same key are always kept, so cross-song data can
// never be lost by the de-duplication.
func (db *BoltDB) Set(batch map[model.EncodedKey]model.TableValue) error {
	err := db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(db.fingerprintBucket)

		for k, v := range batch {
			rawKey := k.Bytes()
			rawValue := v.Bytes()
			existing := b.Get(rawKey)

			// Skip the write if this exact record already belongs to a song.
			// Multiple songs can still share the same key: their records are
			// appended to the value list as long as they differ.
			if containsTableValue(existing, rawValue) {
				continue
			}

			// Copy the existing records into a fresh slice instead of appending
			// to the bbolt-managed buffer directly: b.Get returns a read-only
			// view over the memory mapped file that must never be modified.
			updated := make([]byte, 0, len(existing)+len(rawValue))
			updated = append(updated, existing...)
			updated = append(updated, rawValue...)

			if err := b.Put(rawKey, updated); err != nil {
				return errors.Wrapf(err, "error setting (key: %v, val: %v)", k, v)
			}
		}
		return nil
	})

	return errors.Wrap(err, "an error occurred when writing to bolt")
}

// containsTableValue reports whether the serialized value list raw contains
// the serialized TableValue rawValue (each record is model.TableValueSize bytes).
func containsTableValue(raw, rawValue []byte) bool {
	for off := 0; off+model.TableValueSize <= len(raw); off += model.TableValueSize {
		if bytes.Equal(raw[off:off+model.TableValueSize], rawValue) {
			return true
		}
	}
	return false
}

// GetSongID does a song name => songID lookup in the database
func (db *BoltDB) GetSongID(name string) (uint32, error) {
	var id uint32

	err := db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(db.songBucket)
		c := b.Cursor()

		// TODO: fixme this is not really efficient
		// Iterate over all the song records
		for k, v := c.First(); k != nil; k, v = c.Next() {
			if string(v) == name {
				id = binary.LittleEndian.Uint32(k)
				return nil
			}
		}

		return fmt.Errorf("could not find id for song name: %s", name)
	})

	return id, errors.Wrap(err, "an error occurred when reading from bolt")
}

// GetSong does a songID => song name lookup in the database
func (db *BoltDB) GetSong(songID uint32) (string, error) {
	var name string

	err := db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(db.songBucket)

		raw := b.Get(itob(songID))
		if len(raw) == 0 {
			return fmt.Errorf("got empty song name")
		}

		name = string(raw)

		return nil
	})

	return name, errors.Wrap(err, "an error occurred when reading from bolt")
}

// SetSong stores a song name in the database and returns its song ID.
// It is idempotent: if the song is already registered, its existing ID is
// returned and no new ID is ever allocated. The lookup and the registration
// happen in a single transaction so concurrent calls for the same song cannot
// create duplicate registrations.
func (db *BoltDB) SetSong(song string) (uint32, error) {
	var songID uint32

	err := db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(db.songBucket)
		if err != nil {
			return errors.Wrap(err, "error creating bucket")
		}

		// Reuse the existing ID when the song is already registered.
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			if string(v) == song {
				songID = binary.LittleEndian.Uint32(k)
				return nil
			}
		}

		id, err := b.NextSequence()
		if err != nil {
			return errors.Wrap(err, "error getting next sequence")
		}

		songID = uint32(id)
		return errors.Wrap(b.Put(itob(songID), []byte(song)), "error setting song")
	})

	return songID, errors.Wrap(err, "an error occurred when writing to bolt")
}

func itob(s uint32) []byte {
	raw := make([]byte, 4)
	binary.LittleEndian.PutUint32(raw, s)
	return raw
}
