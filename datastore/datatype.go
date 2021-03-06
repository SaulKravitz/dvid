/*
	This file contains code useful for arbitrary data types supported in DVID.
*/

package datastore

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/janelia-flyem/dvid/dvid"
	"github.com/janelia-flyem/dvid/storage"
)

// This message is used for all data types to explain options.
const helpMessage = `
    DVID data type information

    name: %s 
    url: %s 
`

type UrlString string

// TypeID provides methods for determining the identity of a data type.
type TypeID interface {
	// TypeName is an abbreviated type name.
	DatatypeName() dvid.TypeString

	// TypeUrl returns the unique package name that fulfills the DVID Data interface
	DatatypeUrl() UrlString

	// TypeVersion describes the version identifier of this data type code
	DatatypeVersion() string
}

// TypeService is an interface for operations using arbitrary data types.
type TypeService interface {
	TypeID

	// Help returns a string explaining how to use a data type's service
	Help() string

	// Create Data that is an instance of this data type in the given Dataset
	NewDataService(id *DataID, config dvid.Config) (service DataService, err error)
}

// Subsetter is a type that can tell us its range of Index and how much it has
// actually available in this server.  It's used to implement limited cloning,
// e.g., only cloning a quarter of an image volume.
// TODO: Fulfill implementation for voxels data type.
type Subsetter interface {
	// MaximumExtents returns a range of indices for which data is available at
	// some DVID server.
	MaximumExtents() dvid.IndexRange

	// AvailableExtents returns a range of indices for which data is available
	// at this DVID server.  It is the currently available extents.
	AvailableExtents() dvid.IndexRange
}

// DataService is an interface for operations on arbitrary data that
// use a supported TypeService.  Chunk handlers are allocated at this level,
// so an implementation can own a number of goroutines.
//
// DataService operations are completely type-specific, and each datatype
// handles operations through RPC (DoRPC) and HTTP (DoHTTP).
// TODO -- Add SPDY as wrapper to HTTP.
type DataService interface {
	TypeService

	// DataName returns the name of the data (e.g., grayscale data that is grayscale8 data type).
	DataName() dvid.DataString

	// IsVersioned returns true if this data can be mutated across versions.  If the data is
	// not versioned, only one copy of data is kept across all versions nodes in a dataset.
	IsVersioned() bool

	// ModifyConfig modifies a configuration in a type-specific way.
	ModifyConfig(config dvid.Config) error

	// DoRPC handles command line and RPC commands specific to a data type
	DoRPC(request Request, reply *Response) error

	// DoHTTP handles HTTP requests specific to a data type
	DoHTTP(uuid dvid.UUID, w http.ResponseWriter, r *http.Request) error

	// Returns standard error response for unknown commands
	UnknownCommand(r Request) error
}

// Request supports requests to the DVID server.
type Request struct {
	dvid.Command
	Input []byte
}

var (
	HelpRequest = Request{Command: []string{"help"}}
)

// Response supports responses from DVID.
type Response struct {
	dvid.Response
	Output []byte
}

// Writes a response to a writer.
func (r *Response) Write(w io.Writer) error {
	if len(r.Response.Text) != 0 {
		fmt.Fprintf(w, r.Response.Text)
		return nil
	} else if len(r.Output) != 0 {
		_, err := w.Write(r.Output)
		if err != nil {
			return err
		}
	}
	return nil
}

// CompiledTypes is the set of registered data types compiled into DVID and
// held as a global variable initialized at runtime.
var CompiledTypes = map[UrlString]TypeService{}

// CompiledTypeNames returns a list of data type names compiled into this DVID.
func CompiledTypeNames() string {
	var names []string
	for _, datatype := range CompiledTypes {
		names = append(names, string(datatype.DatatypeName()))
	}
	return strings.Join(names, ", ")
}

// CompiledTypeUrls returns a list of data type urls supported by this DVID.
func CompiledTypeUrls() string {
	var urls []string
	for url, _ := range CompiledTypes {
		urls = append(urls, string(url))
	}
	return strings.Join(urls, ", ")
}

// CompiledTypeChart returns a chart (names/urls) of data types compiled into this DVID.
func CompiledTypeChart() string {
	var text string = "\nData types compiled into this DVID\n\n"
	writeLine := func(name dvid.TypeString, url UrlString) {
		text += fmt.Sprintf("%-15s   %s\n", name, url)
	}
	writeLine("Name", "Url")
	for _, datatype := range CompiledTypes {
		writeLine(datatype.DatatypeName(), datatype.DatatypeUrl())
	}
	return text + "\n"
}

// RegisterDatatype registers a data type for DVID use.
func RegisterDatatype(t TypeService) {
	if CompiledTypes == nil {
		CompiledTypes = make(map[UrlString]TypeService)
	}
	CompiledTypes[t.DatatypeUrl()] = t
}

// TypeServiceByName returns a type-specific service given a type name.
func TypeServiceByName(typeName dvid.TypeString) (typeService TypeService, err error) {
	for _, dtype := range CompiledTypes {
		if typeName == dtype.DatatypeName() {
			typeService = dtype
			return
		}
	}
	err = fmt.Errorf("Data type '%s' is not supported in current DVID executable", typeName)
	return
}

// ---- TypeService Implementation ----

// DatatypeID uniquely identifies a DVID-supported data type and provides a
// shorthand name.
type DatatypeID struct {
	// Data type name and may not be unique.
	Name dvid.TypeString

	// The unique package name that fulfills the DVID Data interface
	Url UrlString

	// The version identifier of this data type code
	Version string
}

func MakeDatatypeID(name dvid.TypeString, url UrlString, version string) *DatatypeID {
	return &DatatypeID{name, url, version}
}

func (id *DatatypeID) DatatypeName() dvid.TypeString { return id.Name }

func (id *DatatypeID) DatatypeUrl() UrlString { return id.Url }

func (id *DatatypeID) DatatypeVersion() string { return id.Version }

// Datatype is the base struct that satisfies a TypeService and can be embedded
// in other data types.
type Datatype struct {
	*DatatypeID

	// A list of interface requirements for the backend datastore
	Requirements *storage.Requirements
}

// The following functions supply standard operations necessary across all supported
// data types and are centralized here for DRY reasons.  Each supported data type
// embeds the datastore.Datatype type and gets these functions for free.

// Types must add a NewData() function...

// NewDataService returns a Data instance.  If the configuration doesn't explicitly
// set compression and checksum, LZ4 and the default checksum (chosen by -crc32 flag)
// is used.
func NewDataService(id *DataID, t TypeService, config dvid.Config) (*Data, error) {
	compression, _ := dvid.NewCompression(dvid.LZ4, dvid.DefaultCompression)
	data := &Data{
		DataID:      id,
		TypeService: t,
		Compression: compression,
		Checksum:    dvid.DefaultChecksum,
	}
	err := data.ModifyConfig(config)
	return data, err
}

func (datatype *Datatype) Help() string {
	return fmt.Sprintf(helpMessage, datatype.Name, datatype.Url)
}

// ---- DataService implementation ----

// DataID identifies data within a DVID server.
type DataID struct {
	Name   dvid.DataString
	ID     dvid.DataLocalID
	DsetID dvid.DatasetLocalID
}

func (id DataID) DataName() dvid.DataString { return id.Name }

func (id DataID) LocalID() dvid.DataLocalID { return id.ID }

func (id DataID) DatasetID() dvid.DatasetLocalID { return id.DsetID }

// Data is an instance of a data type with some identifiers and it satisfies
// a DataService interface.  Each Data is dataset-specific.
type Data struct {
	*DataID
	TypeService

	// Compression of serialized data, e.g., the value in a key-value.
	Compression dvid.Compression

	// Checksum approach for serialized data.
	Checksum dvid.Checksum

	// If false (default), we allow changes along nodes.
	Unversioned bool
}

func (d *Data) UseCompression() dvid.Compression {
	return d.Compression
}

func (d *Data) UseChecksum() dvid.Checksum {
	return d.Checksum
}

func (d *Data) IsVersioned() bool {
	return !d.Unversioned
}

func (d *Data) ModifyConfig(config dvid.Config) error {
	versioned, err := config.IsVersioned()
	if err != nil {
		return err
	}
	d.Unversioned = !versioned

	// Set compression for this instance
	s, found, err := config.GetString("Compression")
	if err != nil {
		return err
	}
	if found {
		format := strings.ToLower(s)
		switch format {
		case "none":
			d.Compression, _ = dvid.NewCompression(dvid.Uncompressed, dvid.DefaultCompression)
		case "snappy":
			d.Compression, _ = dvid.NewCompression(dvid.Snappy, dvid.DefaultCompression)
		case "lz4":
			d.Compression, _ = dvid.NewCompression(dvid.LZ4, dvid.DefaultCompression)
		case "gzip":
			d.Compression, _ = dvid.NewCompression(dvid.Gzip, dvid.DefaultCompression)
		default:
			// Check for gzip + compression level
			parts := strings.Split(format, ":")
			if len(parts) == 2 && parts[0] == "gzip" {
				level, err := strconv.Atoi(parts[1])
				if err != nil {
					return fmt.Errorf("Unable to parse gzip compression level ('%d').  Should be 'gzip:<level>'.", parts[1])
				}
				d.Compression, _ = dvid.NewCompression(dvid.Gzip, dvid.CompressionLevel(level))
			} else {
				return fmt.Errorf("Illegal compression specified: %s", s)
			}
		}
	}

	// Set checksum for this instance
	s, found, err = config.GetString("Checksum")
	if err != nil {
		return err
	}
	if found {
		checksum := strings.ToLower(s)
		switch checksum {
		case "none":
			d.Checksum = dvid.NoChecksum
		case "crc32":
			d.Checksum = dvid.CRC32
		default:
			return fmt.Errorf("Illegal checksum specified: %s", s)
		}
	}
	return nil
}

func (d *Data) UnknownCommand(request Request) error {
	return fmt.Errorf("Unknown command.  Data type '%s' [%s] does not support '%s' command.",
		d.Name, d.DatatypeName(), request.TypeCommand())
}

// --- Handle version-specific data mutexes -----

type nodeID struct {
	Dataset dvid.DatasetLocalID
	Data    dvid.DataLocalID
	Version dvid.VersionLocalID
}

// Map of mutexes at the granularity of dataset/data/version
var versionMutexes map[nodeID]*sync.Mutex

func init() {
	versionMutexes = make(map[nodeID]*sync.Mutex)
}

// VersionMutex returns a Mutex that is specific for data at a particular version.
func (d *Data) VersionMutex(versionID dvid.VersionLocalID) *sync.Mutex {
	var mutex sync.Mutex
	mutex.Lock()
	id := nodeID{d.DsetID, d.ID, versionID}
	vmutex, found := versionMutexes[id]
	if !found {
		vmutex = new(sync.Mutex)
		versionMutexes[id] = vmutex
	}
	mutex.Unlock()
	return vmutex
}
