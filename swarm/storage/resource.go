package storage

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/net/idna"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/contracts/ens"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
)

const (
	signatureLength         = 65
	metadataChunkOffsetSize = 18 // size of fixed-length portion of metadata chunk; 0x0000 || startblock || frequency
	DbDirName               = "resource"
	chunkSize               = 4096 // temporary until we implement DPA in the resourcehandler
	defaultStoreTimeout     = 4000 * time.Millisecond
	hasherCount             = 8
	resourceHash            = SHA3Hash
	defaultRetrieveTimeout  = 100 * time.Millisecond
)

type blockEstimator struct {
	Start   time.Time
	Average time.Duration
}

// TODO: Average must  be adjusted when blockchain connection is present and synced
func NewBlockEstimator() *blockEstimator {
	sampleDate, _ := time.Parse(time.RFC3339, "2018-05-04T20:35:22Z")   // from etherscan.io
	sampleBlock := int64(3169691)                                       // from etherscan.io
	ropstenStart, _ := time.Parse(time.RFC3339, "2016-11-20T11:48:50Z") // from etherscan.io
	ns := sampleDate.Sub(ropstenStart).Nanoseconds()
	period := int(ns / sampleBlock)
	parsestring := fmt.Sprintf("%dns", int(float64(period)*1.0005)) // increase the blockcount a little, so we don't overshoot the read block height; if we do, we will never find the updates when getting synced data
	periodNs, _ := time.ParseDuration(parsestring)
	return &blockEstimator{
		Start:   ropstenStart,
		Average: periodNs,
	}
}

func (b *blockEstimator) HeaderByNumber(context.Context, string, *big.Int) (*types.Header, error) {
	return &types.Header{
		Number: big.NewInt(time.Since(b.Start).Nanoseconds() / b.Average.Nanoseconds()),
	}, nil
}

type ResourceError struct {
	code int
	err  string
}

func (e *ResourceError) Error() string {
	return e.err
}

func (e *ResourceError) Code() int {
	return e.code
}

func NewResourceError(code int, s string) error {
	if code < 0 || code >= ErrCnt {
		panic("no such error code!")
	}
	r := &ResourceError{
		err: s,
	}
	switch code {
	case ErrNotFound, ErrIO, ErrUnauthorized, ErrInvalidValue, ErrDataOverflow, ErrNothingToReturn, ErrInvalidSignature, ErrNotSynced, ErrPeriodDepth, ErrCorruptData:
		r.code = code
	}
	return r
}

type Signature [signatureLength]byte

type ResourceLookupParams struct {
	Limit bool
	Max   uint32
}

// Encapsulates an specific resource update. When synced it contains the most recent
// version of the resource update data.
type resource struct {
	*bytes.Reader
	Multihash  bool
	name       string
	nameHash   common.Hash
	startBlock uint64
	lastPeriod uint32
	lastKey    Key
	frequency  uint64
	version    uint32
	data       []byte
	updated    time.Time
}

// TODO Expire content after a defined period (to force resync)
func (self *resource) isSynced() bool {
	return !self.updated.IsZero()
}

func (self *resource) NameHash() common.Hash {
	return self.nameHash
}

func (self *resource) Size(chan bool) (int64, error) {
	if !self.isSynced() {
		return 0, NewResourceError(ErrNotSynced, "Not synced")
	}
	return int64(len(self.data)), nil
}

func (self *resource) Name() string {
	return self.name
}

func (self *resource) UnmarshalBinary(data []byte) error {
	self.startBlock = binary.LittleEndian.Uint64(data[:8])
	self.frequency = binary.LittleEndian.Uint64(data[8:16])
	self.name = string(data[16:])
	return nil
}

func (self *resource) MarshalBinary() ([]byte, error) {
	b := make([]byte, 16+len(self.name))
	binary.LittleEndian.PutUint64(b, self.startBlock)
	binary.LittleEndian.PutUint64(b[8:], self.frequency)
	copy(b[16:], []byte(self.name))
	return b, nil
}

type headerGetter interface {
	HeaderByNumber(context.Context, string, *big.Int) (*types.Header, error)
}

type ownerValidator interface {
	ValidateOwner(name string, address common.Address) (bool, error)
}

// Mutable resource is an entity which allows updates to a resource
// without resorting to ENS on each update.
// The update scheme is built on swarm chunks with chunk keys following
// a predictable, versionable pattern.
//
// Updates are defined to be periodic in nature, where periods are
// expressed in terms of number of blocks.
//
// The root entry of a mutable resource is tied to a unique identifier,
// typically - but not necessarily - an ens name.  The identifier must be
// an valid IDNA string. It also contains the block number
// when the resource update was first registered, and
// the block frequency with which the resource will be updated, both of
// which are stored as little-endian uint64 values in the database (for a
// total of 16 bytes). It also contains the unique identifier.
// It is stored in a separate content-addressed chunk (call it the metadata chunk),
// with the following layout:
//
// (0x0000|startblock|frequency|identifier)
//
// (The two first zero-value bytes are used for disambiguation by the chunk validator,
// and update chunk will always have a value > 0 there.)
//
// The root entry tells the requester from when the mutable resource was
// first added (block number) and in which block number to look for the
// actual updates. Thus, a resource update for identifier "føø.bar"
// starting at block 4200 with frequency 42 will have updates on block 4242,
// 4284, 4326 and so on.
//
// Actual data updates are also made in the form of swarm chunks. The keys
// of the updates are the hash of a concatenation of properties as follows:
//
// sha256(period|version|namehash)
//
// The period is (currentblock - startblock) / frequency
//
// Using our previous example, this means that a period 3 will have 4326 as
// the block number.
//
// If more than one update is made to the same block number, incremental
// version numbers are used successively.
//
// A lookup agent need only know the identifier name in order to get the versions
//
// the resourcedata is:
// headerlength|period|version|identifier|data
//
// if a validator is active, the chunk data is:
// resourcedata|sign(resourcedata)
// otherwise, the chunk data is the same as the resourcedata
//
// headerlength is a 16 bit value containing the byte length of period|version|name
//
// TODO: Include modtime in chunk data + signature
type ResourceHandler struct {
	chunkStore      *NetStore
	HashSize        int
	signer          ResourceSigner
	headerGetter    headerGetter
	ownerValidator  ownerValidator
	resources       map[string]*resource
	hashPool        sync.Pool
	resourceLock    sync.RWMutex
	storeTimeout    time.Duration
	queryMaxPeriods *ResourceLookupParams
}

type ResourceHandlerParams struct {
	QueryMaxPeriods *ResourceLookupParams
	Signer          ResourceSigner
	HeaderGetter    headerGetter
	OwnerValidator  ownerValidator
}

// Create or open resource update chunk store
func NewResourceHandler(params *ResourceHandlerParams) (*ResourceHandler, error) {
	if params.QueryMaxPeriods == nil {
		params.QueryMaxPeriods = &ResourceLookupParams{
			Limit: false,
		}
	}
	rh := &ResourceHandler{
		headerGetter:   params.HeaderGetter,
		ownerValidator: params.OwnerValidator,
		resources:      make(map[string]*resource),
		storeTimeout:   defaultStoreTimeout,
		signer:         params.Signer,
		hashPool: sync.Pool{
			New: func() interface{} {
				return MakeHashFunc(resourceHash)()
			},
		},
		queryMaxPeriods: params.QueryMaxPeriods,
	}

	for i := 0; i < hasherCount; i++ {
		hashfunc := MakeHashFunc(resourceHash)()
		if rh.HashSize == 0 {
			rh.HashSize = hashfunc.Size()
		}
		rh.hashPool.Put(hashfunc)
	}

	return rh, nil
}

// Sets the store backend for resource updates
func (self *ResourceHandler) SetStore(store *NetStore) {
	self.chunkStore = store
}

// Chunk Validation method (matches ChunkValidatorFunc signature)
//
// If resource update, owner is checked against ENS record of resource name inferred from chunk data
// If parsed signature is nil, validates automatically
// If not resource update, it validates are metadata chunk if length is metadataChunkOffsetSize and first two bytes are 0
func (self *ResourceHandler) Validate(key Key, data []byte) bool {
	signature, period, version, name, parseddata, _, err := self.parseUpdate(data)
	if err != nil {
		if len(data) > metadataChunkOffsetSize { // identifier comes after this byte range, and must be at least one byte
			if bytes.Equal(data[:2], []byte{0, 0}) {
				return true
			}
		}
		log.Error("Invalid resource chunk")
		return false
	} else if signature == nil {
		return bytes.Equal(self.resourceHash(period, version, ens.EnsNode(name)), key)
	}

	digest := self.keyDataHash(key, parseddata)
	addr, err := getAddressFromDataSig(digest, *signature)
	if err != nil {
		log.Error("Invalid signature on resource chunk")
		return false
	}
	ok, _ := self.checkAccess(name, addr)
	return ok
}

// If no ens client is supplied, resource updates are not validated
func (self *ResourceHandler) IsValidated() bool {
	return self.ownerValidator != nil
}

// Create the resource update digest used in signatures
func (self *ResourceHandler) keyDataHash(key Key, data []byte) common.Hash {
	hasher := self.hashPool.Get().(SwarmHash)
	defer self.hashPool.Put(hasher)
	hasher.Reset()
	hasher.Write(key[:])
	hasher.Write(data)
	return common.BytesToHash(hasher.Sum(nil))
}

// Checks if current address matches owner address of ENS
func (self *ResourceHandler) checkAccess(name string, address common.Address) (bool, error) {
	if self.ownerValidator == nil {
		return true, nil
	}
	return self.ownerValidator.ValidateOwner(name, address)
}

// Get the currently loaded data from the resource
func (self *ResourceHandler) GetContent(nameHash string) (string, []byte, error) {
	rsrc := self.getResource(nameHash)
	if rsrc == nil {
		return "", nil, NewResourceError(ErrNotFound, "Resource does not exist")
	} else if !rsrc.isSynced() {
		return "", nil, NewResourceError(ErrNotSynced, "Resource is not synced")
	}
	return rsrc.name, rsrc.data, nil
}

// Gets the period of the current data loaded in the resource
func (self *ResourceHandler) GetLastPeriod(nameHash string) (uint32, error) {
	rsrc := self.getResource(nameHash)
	if rsrc == nil {
		return 0, NewResourceError(ErrNotFound, "Resource does not exist")
	} else if !rsrc.isSynced() {
		return 0, NewResourceError(ErrNotSynced, "Resource is not synced")
	}
	return rsrc.lastPeriod, nil
}

// Gets the version of the current data loaded in the resource
func (self *ResourceHandler) GetVersion(nameHash string) (uint32, error) {
	rsrc := self.getResource(nameHash)
	if rsrc == nil {
		return 0, NewResourceError(ErrNotFound, "Resource does not exist")
	} else if !rsrc.isSynced() {
		return 0, NewResourceError(ErrNotSynced, "Resource is not synced")
	}
	return rsrc.version, nil
}

// \TODO should be hashsize * branches from the chosen chunker, implement with dpa
func (self *ResourceHandler) chunkSize() int64 {
	return chunkSize
}

// Creates a new root entry for a mutable resource identified by `name` with the specified `frequency`.
//
// The signature data should match the hash of the idna-converted name by the validator's namehash function, NOT the raw name bytes.
//
// The start block of the resource update will be the actual current block height of the connected network.
func (self *ResourceHandler) NewResource(ctx context.Context, name string, frequency uint64) (Key, *resource, error) {

	// frequency 0 is invalid
	if frequency == 0 {
		return nil, nil, NewResourceError(ErrInvalidValue, "Frequency cannot be 0")
	}

	// make sure name only contains ascii values
	if !isSafeName(name) {
		return nil, nil, NewResourceError(ErrInvalidValue, fmt.Sprintf("Invalid name: '%s'", name))
	}

	nameHash := ens.EnsNode(name)

	// if the signer function is set, validate that the key of the signer has access to modify this ENS name
	if self.signer != nil {
		signature, err := self.signer.Sign(nameHash)
		if err != nil {
			return nil, nil, NewResourceError(ErrInvalidSignature, fmt.Sprintf("Sign fail: %v", err))
		}
		addr, err := getAddressFromDataSig(nameHash, signature)
		if err != nil {
			return nil, nil, NewResourceError(ErrInvalidSignature, fmt.Sprintf("Retrieve address from signature fail: %v", err))
		}
		ok, err := self.checkAccess(name, addr)
		if err != nil {
			return nil, nil, err
		} else if !ok {
			return nil, nil, NewResourceError(ErrUnauthorized, fmt.Sprintf("Not owner of '%s'", name))
		}
	}

	// get our blockheight at this time
	currentblock, err := self.getBlock(ctx, name)
	if err != nil {
		return nil, nil, err
	}

	chunk := self.newMetaChunk(name, currentblock, frequency)

	self.chunkStore.Put(chunk)
	log.Debug("new resource", "name", name, "key", nameHash, "startBlock", currentblock, "frequency", frequency)

	// create the internal index for the resource and populate it with the data of the first version
	rsrc := &resource{
		startBlock: currentblock,
		frequency:  frequency,
		name:       name,
		nameHash:   nameHash,
		updated:    time.Now(),
	}
	self.setResource(nameHash.Hex(), rsrc)

	return chunk.Key, rsrc, nil
}

func (self *ResourceHandler) newMetaChunk(name string, startBlock uint64, frequency uint64) *Chunk {
	// the metadata chunk points to data of first blockheight + update frequency
	// from this we know from what blockheight we should look for updates, and how often
	// it also contains the name of the resource, so we know what resource we are working with
	data := make([]byte, metadataChunkOffsetSize+len(name))

	// root block has first two bytes both set to 0, which distinguishes from update bytes
	val := make([]byte, 8)
	binary.LittleEndian.PutUint64(val, startBlock)
	copy(data[2:10], val)
	binary.LittleEndian.PutUint64(val, frequency)
	copy(data[10:18], val)
	copy(data[18:], []byte(name))

	// the key of the metadata chunk is content-addressed
	// if it wasn't we couldn't replace it later
	// resolving this relationship is left up to external agents (for example ENS)
	hasher := self.hashPool.Get().(SwarmHash)
	hasher.Reset()
	hasher.Write(data)
	key := hasher.Sum(nil)
	self.hashPool.Put(hasher)

	// make the chunk and send it to swarm
	chunk := NewChunk(key, nil)
	chunk.SData = make([]byte, metadataChunkOffsetSize+len(name))
	copy(chunk.SData, data)
	return chunk
}

// Searches and retrieves the specific version of the resource update identified by `name`
// at the specific block height
//
// If refresh is set to true, the resource data will be reloaded from the resource update
// metadata chunk.
// It is the callers responsibility to make sure that this chunk exists (if the resource
// update root data was retrieved externally, it typically doesn't)
func (self *ResourceHandler) LookupVersionByName(ctx context.Context, name string, period uint32, version uint32, refresh bool, maxLookup *ResourceLookupParams) (*resource, error) {
	return self.LookupVersion(ctx, ens.EnsNode(name), period, version, refresh, maxLookup)
}

func (self *ResourceHandler) LookupVersion(ctx context.Context, nameHash common.Hash, period uint32, version uint32, refresh bool, maxLookup *ResourceLookupParams) (*resource, error) {
	rsrc := self.getResource(nameHash.Hex())
	if rsrc == nil {
		return nil, NewResourceError(ErrNothingToReturn, "resource not loaded")
	}
	return self.lookup(rsrc, period, version, refresh, maxLookup)
}

// Retrieves the latest version of the resource update identified by `name`
// at the specified block height
//
// If an update is found, version numbers are iterated until failure, and the last
// successfully retrieved version is copied to the corresponding resources map entry
// and returned.
//
// See also (*ResourceHandler).LookupVersion
func (self *ResourceHandler) LookupHistoricalByName(ctx context.Context, name string, period uint32, refresh bool, maxLookup *ResourceLookupParams) (*resource, error) {
	return self.LookupHistorical(ctx, ens.EnsNode(name), period, refresh, maxLookup)
}

func (self *ResourceHandler) LookupHistorical(ctx context.Context, nameHash common.Hash, period uint32, refresh bool, maxLookup *ResourceLookupParams) (*resource, error) {
	rsrc := self.getResource(nameHash.Hex())
	if rsrc == nil {
		return nil, NewResourceError(ErrNothingToReturn, "resource not loaded")
	}
	return self.lookup(rsrc, period, 0, refresh, maxLookup)
}

// Retrieves the latest version of the resource update identified by `name`
// at the next update block height
//
// It starts at the next period after the current block height, and upon failure
// tries the corresponding keys of each previous period until one is found
// (or startBlock is reached, in which case there are no updates).
//
// Version iteration is done as in (*ResourceHandler).LookupHistorical
//
// See also (*ResourceHandler).LookupHistorical
func (self *ResourceHandler) LookupLatestByName(ctx context.Context, name string, refresh bool, maxLookup *ResourceLookupParams) (*resource, error) {
	return self.LookupLatest(ctx, ens.EnsNode(name), refresh, maxLookup)
}

func (self *ResourceHandler) LookupLatest(ctx context.Context, nameHash common.Hash, refresh bool, maxLookup *ResourceLookupParams) (*resource, error) {

	// get our blockheight at this time and the next block of the update period
	rsrc := self.getResource(nameHash.Hex())
	if rsrc == nil {
		return nil, NewResourceError(ErrNothingToReturn, "resource not loaded")
	}
	currentblock, err := self.getBlock(ctx, rsrc.name)
	if err != nil {
		return nil, err
	}
	nextperiod, err := getNextPeriod(rsrc.startBlock, currentblock, rsrc.frequency)
	if err != nil {
		return nil, err
	}
	return self.lookup(rsrc, nextperiod, 0, refresh, maxLookup)
}

// Returns the resource before the one currently loaded in the resource index
//
// This is useful where resource updates are used incrementally in contrast to
// merely replacing content.
//
// Requires a synced resource object
func (self *ResourceHandler) LookupPreviousByName(ctx context.Context, name string, maxLookup *ResourceLookupParams) (*resource, error) {
	return self.LookupPrevious(ctx, ens.EnsNode(name), maxLookup)
}

func (self *ResourceHandler) LookupPrevious(ctx context.Context, nameHash common.Hash, maxLookup *ResourceLookupParams) (*resource, error) {
	rsrc := self.getResource(nameHash.Hex())
	if rsrc == nil {
		return nil, NewResourceError(ErrNothingToReturn, "resource not loaded")
	}
	if !rsrc.isSynced() {
		return nil, NewResourceError(ErrNotSynced, "LookupPrevious requires synced resource.")
	} else if rsrc.lastPeriod == 0 {
		return nil, NewResourceError(ErrNothingToReturn, "Resource not found")
	}
	if rsrc.version > 1 {
		rsrc.version--
	} else if rsrc.lastPeriod == 1 {
		return nil, NewResourceError(ErrNothingToReturn, "Current update is the oldest")
	} else {
		rsrc.version = 0
		rsrc.lastPeriod--
	}
	return self.lookup(rsrc, rsrc.lastPeriod, rsrc.version, false, maxLookup)
}

// base code for public lookup methods
func (self *ResourceHandler) lookup(rsrc *resource, period uint32, version uint32, refresh bool, maxLookup *ResourceLookupParams) (*resource, error) {

	// we can't look for anything without a store
	if self.chunkStore == nil {
		return nil, NewResourceError(ErrInit, "Call ResourceHandler.SetStore() before performing lookups")
	}

	// period 0 does not exist
	if period == 0 {
		return nil, NewResourceError(ErrInvalidValue, "period must be >0")
	}

	// start from the last possible block period, and iterate previous ones until we find a match
	// if we hit startBlock we're out of options
	var specificversion bool
	if version > 0 {
		specificversion = true
	} else {
		version = 1
	}

	var hops uint32
	if maxLookup == nil {
		maxLookup = self.queryMaxPeriods
	}
	log.Trace("resource lookup", "period", period, "version", version, "limit", maxLookup.Limit, "max", maxLookup.Max)
	for period > 0 {
		if maxLookup.Limit && hops > maxLookup.Max {
			return nil, NewResourceError(ErrPeriodDepth, fmt.Sprintf("Lookup exceeded max period hops (%d)", maxLookup.Max))
		}
		key := self.resourceHash(period, version, rsrc.nameHash)
		chunk, err := self.chunkStore.get(key, defaultRetrieveTimeout)
		if err == nil {
			if specificversion {
				return self.updateResourceIndex(rsrc, chunk)
			}
			// check if we have versions > 1. If a version fails, the previous version is used and returned.
			log.Trace("rsrc update version 1 found, checking for version updates", "period", period, "key", key)
			for {
				newversion := version + 1
				key := self.resourceHash(period, newversion, rsrc.nameHash)
				newchunk, err := self.chunkStore.get(key, defaultRetrieveTimeout)
				if err != nil {
					return self.updateResourceIndex(rsrc, chunk)
				}
				chunk = newchunk
				version = newversion
				log.Trace("version update found, checking next", "version", version, "period", period, "key", key)
			}
		}
		log.Trace("rsrc update not found, checking previous period", "period", period, "key", key)
		period--
		hops++
	}
	return nil, NewResourceError(ErrNotFound, "no updates found")
}

// Retrieves a resource metadata chunk and creates/updates the index entry for it
// with the resulting metadata
func (self *ResourceHandler) LoadResource(key Key) (*resource, error) {
	chunk, err := self.chunkStore.get(key, defaultRetrieveTimeout)
	if err != nil {
		return nil, NewResourceError(ErrNotFound, err.Error())
	}

	// minimum sanity check for chunk data (an update chunk first two bytes is headerlength uint16, and cannot be 0)
	// \TODO this is not enough to make sure the data isn't bogus. A normal content addressed chunk could still satisfy these criteria
	if !bytes.Equal(chunk.SData[:2], []byte{0x0, 0x0}) {
		return nil, NewResourceError(ErrCorruptData, fmt.Sprintf("Chunk is not a resource metadata chunk"))
	} else if len(chunk.SData) <= metadataChunkOffsetSize {
		return nil, NewResourceError(ErrNothingToReturn, fmt.Sprintf("Invalid chunk length %d, should be minimum %d", len(chunk.SData), metadataChunkOffsetSize+1))
	}

	// create the index entry
	rsrc := &resource{}
	rsrc.UnmarshalBinary(chunk.SData[2:])
	rsrc.nameHash = ens.EnsNode(rsrc.name)
	self.setResource(rsrc.nameHash.Hex(), rsrc)
	log.Trace("resource index load", "rootkey", key, "name", rsrc.name, "namehash", rsrc.nameHash, "startblock", rsrc.startBlock, "frequency", rsrc.frequency)
	return rsrc, nil
}

// update mutable resource index map with content from a retrieved update chunk
func (self *ResourceHandler) updateResourceIndex(rsrc *resource, chunk *Chunk) (*resource, error) {

	// retrieve metadata from chunk data and check that it matches this mutable resource
	signature, period, version, name, data, multihash, err := self.parseUpdate(chunk.SData)
	if rsrc.name != name {
		return nil, NewResourceError(ErrNothingToReturn, fmt.Sprintf("Update belongs to '%s', but have '%s'", name, rsrc.name))
	}
	log.Trace("resource index update", "name", rsrc.name, "namehash", rsrc.nameHash, "updatekey", chunk.Key, "period", period, "version", version)

	// check signature (if signer algorithm is present)
	// \TODO maybe this check is redundant if also checked upon retrieval of chunk
	if signature != nil {
		digest := self.keyDataHash(chunk.Key, data)
		_, err = getAddressFromDataSig(digest, *signature)
		if err != nil {
			return nil, NewResourceError(ErrUnauthorized, fmt.Sprintf("Invalid signature: %v", err))
		}
	}

	// update our rsrcs entry map
	rsrc.lastKey = chunk.Key
	rsrc.lastPeriod = period
	rsrc.version = version
	rsrc.updated = time.Now()
	rsrc.data = make([]byte, len(data))
	rsrc.Multihash = multihash
	rsrc.Reader = bytes.NewReader(rsrc.data)
	copy(rsrc.data, data)
	log.Debug("Resource synced", "name", rsrc.name, "key", chunk.Key, "period", rsrc.lastPeriod, "version", rsrc.version)
	self.setResource(rsrc.nameHash.Hex(), rsrc)
	return rsrc, nil
}

// retrieve update metadata from chunk data
// mirrors newUpdateChunk()
func (self *ResourceHandler) parseUpdate(chunkdata []byte) (*Signature, uint32, uint32, string, []byte, bool, error) {
	// absolute minimum an update chunk can contain:
	// 14 = header + one byte of name + one byte of data
	if len(chunkdata) < 14 {
		return nil, 0, 0, "", nil, false, NewResourceError(ErrNothingToReturn, "chunk less than 13 bytes cannot be a resource update chunk")
	}
	cursor := 0
	headerlength := binary.LittleEndian.Uint16(chunkdata[cursor : cursor+2])
	cursor += 2
	datalength := binary.LittleEndian.Uint16(chunkdata[cursor : cursor+2])
	cursor += 2
	var exclsignlength int
	// we need extra magic if it's a multihash, since we used datalength 0 in header as an indicator of multihash content
	// retrieve the second varint and set this as the data length
	// TODO: merge with isMultihash code
	if datalength == 0 {
		uvarintbuf := bytes.NewBuffer(chunkdata[headerlength+4:])
		r, err := binary.ReadUvarint(uvarintbuf)
		if err != nil {
			errstr := fmt.Sprintf("corrupt multihash, hash id varint could not be read: %v", err)
			log.Warn(errstr)
			return nil, 0, 0, "", nil, false, NewResourceError(ErrCorruptData, errstr)

		}
		r, err = binary.ReadUvarint(uvarintbuf)
		if err != nil {
			errstr := fmt.Sprintf("corrupt multihash, hash length field could not be read: %v", err)
			log.Warn(errstr)
			return nil, 0, 0, "", nil, false, NewResourceError(ErrCorruptData, errstr)

		}
		exclsignlength = int(headerlength + uint16(r))
	} else {
		exclsignlength = int(headerlength + datalength + 4)
	}

	// the total length excluding signature is headerlength and datalength fields plus the length of the header and the data given in these fields
	exclsignlength = int(headerlength + datalength + 4)
	if exclsignlength > len(chunkdata) || exclsignlength < 14 {
		return nil, 0, 0, "", nil, false, NewResourceError(ErrNothingToReturn, fmt.Sprintf("Reported headerlength %d + datalength %d longer than actual chunk data length %d", headerlength, exclsignlength, len(chunkdata)))
	} else if exclsignlength < 14 {
		return nil, 0, 0, "", nil, false, NewResourceError(ErrNothingToReturn, fmt.Sprintf("Reported headerlength %d + datalength %d is smaller than minimum valid resource chunk length %d", headerlength, datalength, 14))
	}

	// at this point we can be satisfied that the data integrity is ok
	var period uint32
	var version uint32
	var name string
	var data []byte
	period = binary.LittleEndian.Uint32(chunkdata[cursor : cursor+4])
	cursor += 4
	version = binary.LittleEndian.Uint32(chunkdata[cursor : cursor+4])
	cursor += 4
	namelength := int(headerlength) - cursor + 4
	name = string(chunkdata[cursor : cursor+namelength])
	cursor += namelength

	// if multihash content is indicated we check the validity of the multihash
	// \TODO the check above for multihash probably is sufficient also for this case (or can be with a small adjustment) and if so this code should be removed
	var intdatalength int
	var multihash bool
	if datalength == 0 {
		intdatalength = isMultihash(chunkdata[cursor:])
		multihashboundary := cursor + intdatalength
		if len(chunkdata) != multihashboundary && len(chunkdata) < multihashboundary+signatureLength {
			log.Debug("multihash error", "chunkdatalen", len(chunkdata), "multihashboundary", multihashboundary)
			return nil, 0, 0, "", nil, false, errors.New("Corrupt multihash data")
		}
		multihash = true
	} else {
		intdatalength = int(datalength)
	}
	data = make([]byte, intdatalength)
	copy(data, chunkdata[cursor:cursor+intdatalength])

	// omit signatures if we have no validator
	var signature *Signature
	cursor += intdatalength
	if self.signer != nil {
		sigdata := chunkdata[cursor : cursor+signatureLength]
		if len(sigdata) > 0 {
			signature = &Signature{}
			copy(signature[:], sigdata)
		}
	}

	return signature, period, version, name, data, multihash, nil
}

// Adds an actual data update
//
// Uses the data currently loaded in the resources map entry.
// It is the caller's responsibility to make sure that this data is not stale.
//
// A resource update cannot span chunks, and thus has max length 4096
func (self *ResourceHandler) UpdateMultihash(ctx context.Context, name string, data []byte) (Key, error) {
	// \TODO perhaps this check should be in newUpdateChunk()
	if isMultihash(data) == 0 {
		return nil, NewResourceError(ErrNothingToReturn, "Invalid multihash")
	}
	return self.update(ctx, name, data, true)
}

func (self *ResourceHandler) Update(ctx context.Context, name string, data []byte) (Key, error) {
	return self.update(ctx, name, data, false)
}

// create and commit an update
func (self *ResourceHandler) update(ctx context.Context, name string, data []byte, multihash bool) (Key, error) {

	// zero-length updates are bogus
	if len(data) == 0 {
		return nil, NewResourceError(ErrInvalidValue, "I refuse to waste swarm space for updates with empty values, amigo (data length is 0)")
	}

	// we can't update anything without a store
	if self.chunkStore == nil {
		return nil, NewResourceError(ErrInit, "Call ResourceHandler.SetStore() before updating")
	}

	// signature length is 0 if we are not using them
	var signaturelength int
	if self.signer != nil {
		signaturelength = signatureLength
	}

	// get the cached information
	nameHash := ens.EnsNode(name)
	nameHashHex := nameHash.Hex()
	rsrc := self.getResource(nameHashHex)
	if rsrc == nil {
		return nil, NewResourceError(ErrNotFound, fmt.Sprintf("Resource object '%s' not in index", name))
	} else if !rsrc.isSynced() {
		return nil, NewResourceError(ErrNotSynced, "Resource object not in sync")
	}

	// an update can be only one chunk long; data length less header and signature data
	// 12 = length of header and data length fields (2xuint16) plus period and frequency value fields (2xuint32)
	datalimit := self.chunkSize() - int64(signaturelength-len(name)-12)
	if int64(len(data)) > datalimit {
		return nil, NewResourceError(ErrDataOverflow, fmt.Sprintf("Data overflow: %d / %d bytes", len(data), datalimit))
	}

	// get our blockheight at this time and the next block of the update period
	currentblock, err := self.getBlock(ctx, name)
	if err != nil {
		return nil, NewResourceError(ErrIO, fmt.Sprintf("Could not get block height: %v", err))
	}
	nextperiod, err := getNextPeriod(rsrc.startBlock, currentblock, rsrc.frequency)
	if err != nil {
		return nil, err
	}

	// if we already have an update for this block then increment version
	// resource object MUST be in sync for version to be correct, but we checked this earlier in the method already
	var version uint32
	if self.hasUpdate(nameHashHex, nextperiod) {
		version = rsrc.version
	}
	version++

	// calculate the chunk key
	key := self.resourceHash(nextperiod, version, rsrc.nameHash)

	// if we have a signing function, sign the update
	// \TODO this code should probably be consolidated with corresponding code in NewResource()
	var signature *Signature
	if self.signer != nil {
		// sign the data hash with the key
		digest := self.keyDataHash(key, data)
		sig, err := self.signer.Sign(digest)
		if err != nil {
			return nil, NewResourceError(ErrInvalidSignature, fmt.Sprintf("Sign fail: %v", err))
		}
		signature = &sig

		// get the address of the signer (which also checks that it's a valid signature)
		addr, err := getAddressFromDataSig(digest, *signature)
		if err != nil {
			return nil, NewResourceError(ErrInvalidSignature, fmt.Sprintf("Invalid data/signature: %v", err))
		}
		if self.signer != nil {
			// check if the signer has access to update
			ok, err := self.checkAccess(name, addr)
			if err != nil {
				return nil, NewResourceError(ErrIO, fmt.Sprintf("Access check fail: %v", err))
			} else if !ok {
				return nil, NewResourceError(ErrUnauthorized, fmt.Sprintf("Address %x does not have access to update %s", addr, name))
			}
		}
	}

	// a datalength field set to 0 means the content is a multihash
	var datalength int
	if !multihash {
		datalength = len(data)
	}
	chunk := newUpdateChunk(key, signature, nextperiod, version, name, data, datalength)

	// send the chunk
	self.chunkStore.Put(chunk)
	timeout := time.NewTimer(self.storeTimeout)
	select {
	case <-chunk.dbStoredC:
		if err := chunk.GetErrored(); err != nil {
			return nil, NewResourceError(ErrIO, fmt.Sprintf("chunk not stored: %v", err))
		}
	case <-timeout.C:
		return nil, NewResourceError(ErrIO, "chunk store timeout")
	}
	log.Trace("resource update", "name", name, "key", key, "currentblock", currentblock, "lastperiod", nextperiod, "version", version, "data", chunk.SData, "multihash", multihash)

	// update our resources map entry and return the new key
	rsrc.lastPeriod = nextperiod
	rsrc.version = version
	rsrc.data = make([]byte, len(data))
	copy(rsrc.data, data)
	return key, nil
}

// Closes the datastore.
// Always call this at shutdown to avoid data corruption.
func (self *ResourceHandler) Close() {
	self.chunkStore.Close()
}

// gets the current block height
func (self *ResourceHandler) getBlock(ctx context.Context, name string) (uint64, error) {
	blockheader, err := self.headerGetter.HeaderByNumber(ctx, name, nil)
	if err != nil {
		return 0, err
	}
	return blockheader.Number.Uint64(), nil
}

// Calculate the period index (aka major version number) from a given block number
func (self *ResourceHandler) BlockToPeriod(name string, blocknumber uint64) (uint32, error) {
	return getNextPeriod(self.resources[name].startBlock, blocknumber, self.resources[name].frequency)
}

// Calculate the block number from a given period index (aka major version number)
func (self *ResourceHandler) PeriodToBlock(name string, period uint32) uint64 {
	return self.resources[name].startBlock + (uint64(period) * self.resources[name].frequency)
}

// Retrieves the resource index value for the given nameHash
func (self *ResourceHandler) getResource(nameHash string) *resource {
	self.resourceLock.RLock()
	defer self.resourceLock.RUnlock()
	rsrc := self.resources[nameHash]
	return rsrc
}

// Sets the resource index value for the given nameHash
func (self *ResourceHandler) setResource(nameHash string, rsrc *resource) {
	self.resourceLock.Lock()
	defer self.resourceLock.Unlock()
	self.resources[nameHash] = rsrc
}

// Create a new update chunk key
// format is: hash(period|version|namehash)
func (self *ResourceHandler) resourceHash(period uint32, version uint32, namehash common.Hash) Key {
	hasher := self.hashPool.Get().(SwarmHash)
	defer self.hashPool.Put(hasher)
	hasher.Reset()
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, period)
	hasher.Write(b)
	binary.LittleEndian.PutUint32(b, version)
	hasher.Write(b)
	hasher.Write(namehash[:])
	return hasher.Sum(nil)
}

// Checks if we already have an update on this resource, according to the value in the current state of the resource index
func (self *ResourceHandler) hasUpdate(nameHash string, period uint32) bool {
	return self.resources[nameHash].lastPeriod == period
}

func getAddressFromDataSig(datahash common.Hash, signature Signature) (common.Address, error) {
	pub, err := crypto.SigToPub(datahash.Bytes(), signature[:])
	if err != nil {
		return common.Address{}, err
	}
	return crypto.PubkeyToAddress(*pub), nil
}

// create an update chunk
func newUpdateChunk(key Key, signature *Signature, period uint32, version uint32, name string, data []byte, datalength int) *Chunk {

	// no signatures if no validator
	var signaturelength int
	if signature != nil {
		signaturelength = signatureLength
	}

	// prepend version and period to allow reverse lookups
	headerlength := len(name) + 4 + 4

	actualdatalength := len(data)
	chunk := NewChunk(key, nil)
	chunk.SData = make([]byte, 4+signaturelength+headerlength+actualdatalength) // initial 4 are uint16 length descriptors for headerlength and datalength

	// data header length does NOT include the header length prefix bytes themselves
	cursor := 0
	binary.LittleEndian.PutUint16(chunk.SData[cursor:], uint16(headerlength))
	cursor += 2

	// data length
	binary.LittleEndian.PutUint16(chunk.SData[cursor:], uint16(datalength))
	cursor += 2

	// header = period + version + name
	binary.LittleEndian.PutUint32(chunk.SData[cursor:], period)
	cursor += 4

	binary.LittleEndian.PutUint32(chunk.SData[cursor:], version)
	cursor += 4

	namebytes := []byte(name)
	copy(chunk.SData[cursor:], namebytes)
	cursor += len(namebytes)

	// add the data
	copy(chunk.SData[cursor:], data)

	// if signature is present it's the last item in the chunk data
	if signature != nil {
		cursor += actualdatalength
		copy(chunk.SData[cursor:], signature[:])
	}

	chunk.Size = int64(len(chunk.SData))
	return chunk
}

// Helper function to calculate the next update period number from the current block, start block and frequency
func getNextPeriod(start uint64, current uint64, frequency uint64) (uint32, error) {
	if current < start {
		return 0, NewResourceError(ErrInvalidValue, fmt.Sprintf("given current block value %d < start block %d", current, start))
	}
	blockdiff := current - start
	period := blockdiff / frequency
	return uint32(period + 1), nil
}

// ToSafeName is a helper function to create an valid idna of a given resource update name
func ToSafeName(name string) (string, error) {
	return idna.ToASCII(name)
}

// check that name identifiers contain valid bytes
// Strings created using ToSafeName() should satisfy this check
func isSafeName(name string) bool {
	if name == "" {
		return false
	}
	validname, err := idna.ToASCII(name)
	if err != nil {
		return false
	}
	return validname == name
}

// if first byte is the start of a multihash this function will try to parse it
// if successful it returns the length of multihash data, 0 otherwise
func isMultihash(data []byte) int {
	cursor := 0
	_, c := binary.Uvarint(data)
	if c <= 0 {
		log.Warn("Corrupt multihash data, hashtype is unreadable")
		return 0
	}
	cursor += c
	hashlength, c := binary.Uvarint(data[cursor:])
	if c <= 0 {
		log.Warn("Corrupt multihash data, hashlength is unreadable")
		return 0
	}
	cursor += c
	// we cheekily assume hashlength < maxint
	inthashlength := int(hashlength)
	if len(data[cursor:]) < inthashlength {
		log.Warn("Corrupt multihash data, hash does not align with data boundary")
		return 0
	}
	return cursor + inthashlength
}

func NewTestResourceHandler(datadir string, params *ResourceHandlerParams) (*ResourceHandler, error) {
	path := filepath.Join(datadir, DbDirName)
	rh, err := NewResourceHandler(params)
	if err != nil {
		return nil, fmt.Errorf("resource handler create fail: %v", err)
	}
	localstoreparams := NewDefaultLocalStoreParams()
	localstoreparams.Init(path)
	localStore, err := NewLocalStore(localstoreparams, nil)
	if err != nil {
		return nil, fmt.Errorf("localstore create fail, path %s: %v", path, err)
	}
	localStore.Validators = append(localStore.Validators, NewContentAddressValidator(MakeHashFunc(resourceHash)))
	localStore.Validators = append(localStore.Validators, rh)
	dpaStore := NewNetStore(localStore, nil)
	rh.SetStore(dpaStore)
	return rh, nil
}
