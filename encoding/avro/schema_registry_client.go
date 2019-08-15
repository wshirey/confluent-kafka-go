package avro

import (
	"encoding/json"
	"fmt"
	"github.com/confluentinc/confluent-kafka-go/kafka"
	"github.com/golang/groupcache/lru"
	"log"
	"sync"
)

/* Schema Registry API endpoints
*
* ====Schemas====
* Fetch string: schema(escaped) identified by the input id.
* -GET /schemas/ids/{int: id} returns: JSON blob: schema; raises: 404[03], 500[01]
*
* ====Subjects====
* Fetch JSON array str:subject of all registered subjects
* -GET /subjects returns: JSON array string: subjects; raises: 500[01]
* Fetch JSON array int:versions
* GET /subjects/{string: subject}/versions returns: JSON array of int: versions; raises: 404[01], 500[01]
*
* GET /subjects/{string: subject}/versions/{int|string('latest'): version} returns: JSON blob *schemaMetadata*; raises: 404[01, 02], 422[02], 500[01]
* GET /subjects/{string: subject}/versions/{int|string('latest'): version}/schema returns : JSON blob: schema(unescaped); raises: 404, 422, 500[01, 02, 03]
*
* Delete subject and it's associated subject configuration override
* -DELETE /subjects/{string: subject}) returns: JSON array int: version; raises: 404[01], 500[01]
* Delete subject version
* -DELETE /subjects/{string: subject}/versions/{int|str('latest'): version} returns int: deleted version id; raises: 404[01, 02]
*
* Register new schema under subject
* -POST /subjects/{string: subject}/versions returns JSON blob ; raises: 409, 422[01], 500[01, 02, 03]
* Return SchemaMetadata for the subject version (if any) associated with the schema in the request body
* -POST /subjects/{string: subject} returns JSON *schemaMetadata*; raises: 404[01, 03]
*
* ====Comparability====
* Test schema (http body) against configured comparability for subject version
* -POST /compatibility/subjects/{string: subject}/versions/{int:string('latest'): version} returns: JSON bool:is_compatible; raises: 404[01,02], 422[01,02], 500[01]
*
* ====Config====
* Returns global configuration
* -GET /config  returns: JSON string:comparability; raises: 500[01]
* Update global SR config
* -PUT /config returns: JSON string:compatibility; raises: 422[03], 500[01, 03]
* Update subject level override
* -PUT /config/{string: subject} returns: JSON string:compatibility; raises: 422[03], 500[01,03]
* Returns compatibility level of subject
* GET /config/(string: subject) returns: JSON string:compatibility; raises: 404, 500[01]
 */

/* LRU cache of Schemas */
type schemaCache struct {
	*lru.Cache
}

// Return Schema if available in the LRU
// If no Schema matches its added with an initialized Cache entry
func (c schemaCache) get(schema Schema) *subjectRegistry {
	if sd, ok := c.Get(schema); ok {
		return sd.(*subjectRegistry)
	}

	s := &subjectRegistry{
		subjects: make(map[string]*cacheEntry),
	}

	s.Schema = schema

	c.Add(schema, s)
	return s
}

// Version contains subject registration metadata
type Version struct {
	ID      int    `json:"id,omitempty"`
	Version int    `json:"version,omitempty"`
	Schema  Schema `json:"schema,omitempty"`
}

// MarshalJSON implements the json.Marshaler interface
func (sd *Version) MarshalJSON() ([]byte, error) {
	return json.Marshal(&struct {
		ID     int    `json:"id,omitempty"`
		Schema string `json:"schema,omitempty"`
	}{
		sd.ID,
		sd.Schema.String(),
	})
}

// UnmarshalJSON implements the json.Unmarshaller interface
func (sd *Version) UnmarshalJSON(b []byte) error {
	var err error
	var tmp struct{
		ID int `json:"id"`
		Version int `json:"version"`
		Schema  string `json:"schema"`
	}

	err = json.Unmarshal(b, &tmp)

	sd.ID = tmp.ID
	sd.Version = tmp.Version
	if tmp.Schema != "" {
		sd.Schema, err = Parse(tmp.Schema)
	}

	return err
}

/* Cache entry state */
type cacheEntry struct {
	cond  *sync.Cond
	err   error
	state int
}

/* Subject Version and its associated Schema */
//type Version struct {
//	Version int `json:"version,omitempty"`
//	Version
//}

/* Subject->Schema registration table */
type subjectRegistry struct {
	subjects map[string]*cacheEntry
	Version
}

/* Index->Schema registry */
type idRegistry struct {
	*cacheEntry
	Version
}

/* HTTP(S) Schema Registry Client and schema caches */
type client struct {
	sync.Mutex
	restService *restService
	schemaByID  *lru.Cache
	idBySchema  *schemaCache
}

// SchemaRegistryClient is an interface for clients interacting with the Confluent Schema Registry.
// The Schema Registry's REST interface is further explained in Confluent's Schema Registry API documentation
// https://github.com/confluentinc/schema-registry/blob/master/client/src/main/java/io/confluent/kafka/schemaregistry/client/SchemaRegistryClient.java
type SchemaRegistryClient interface {
	Register(subject string, schema Schema) (id int, err error)
	GetByID(id int) (schema Schema, err error)
	GetID(subject string, schema Schema) (SchemaMetadata *Version, err error)
	GetLatestSchemaMetadata(subject string) (*Version, error)
	GetSchemaMetadata(subject string, version int) (*Version, error)
	GetAllVersions(subject string) ([]int, error)
	GetVersion(subject string, schema Schema) (version int, err error)
	GetAllSubjects() ([]string, error)
	DeleteSubject(subject string) ([]int, error)
	DeleteSubjectVersion(subject string, version int) (deletes int, err error)
	GetCompatibility(subject string) (compatibility Compatibility, err error)
	UpdateCompatibility(subject string, update Compatibility) (compatibility Compatibility, err error)
	TestCompatibility(subject string, version int, schema Schema) (compatible bool, err error)
	GetDefaultCompatibility() (compatibility Compatibility, err error)
	UpdateDefaultCompatibility(update Compatibility) (compatibility Compatibility, err error)
}

// NewCachedSchemaRegistryClient returns a SchemaRegistryClient implementation
func NewCachedSchemaRegistryClient(conf kafka.ConfigMap) (SchemaRegistryClient *client, err error) {

	confCopy := conf.Clone()
	restService, err := newRestService(confCopy)

	if err != nil {
		return nil, err
	}

	queueDepth, err := confCopy.Extract("max.cached.schemas", 1000)

	if len(confCopy) != 0 {
		return nil, kafka.NewError(kafka.ErrInvalidConfig, fmt.Errorf("%s\n", confCopy))
	}

	handle := &client{
		restService: restService,
		idBySchema:  &schemaCache{lru.New(queueDepth.(int))},
		schemaByID:  lru.New(queueDepth.(int)),
	}

	handle.idBySchema.OnEvicted = OnEvict
	return handle, nil
}

/* OnEvict Callback executed on cache entry eviction */
func OnEvict(key lru.Key, value interface{}) {
	log.Printf("Evicting entry %+v from cache\n", key)
}

// Register registers Schema aliased with subject
func (c *client) Register(subject string, schema Schema) (id int, err error) {
	c.Lock()
	sd := c.idBySchema.get(schema)

	var s *cacheEntry
	if s = sd.subjects[subject]; s != nil {
		for s.state == 0 && s.err == nil {
			s.cond.Wait()
		}
		c.Unlock()
		return sd.ID, s.err
	}

	s = &cacheEntry{
		cond: sync.NewCond(c),
	}

	sd.subjects[subject] = s

	defer func() {
		c.Unlock()
		s.cond.Broadcast()
	}()
	c.Unlock()

	err = c.restService.handleRequest(newRequest("POST", version, &sd, subject), &sd)

	c.Lock()
	s.state = sd.ID
	s.err = err

	if err != nil {
		log.Printf("schema registration failed for subject %s\n\t%s", subject, err)
		c.idBySchema.Remove(schema)
		return 0, err
	}

	c.idBySchema.Add(schema, sd)
	log.Printf("schema registered successfully with subject %s\n", subject)
	return sd.ID, err
}

// GetById returns the schema identified by id
// Returns Schema object on success
func (c *client) GetByID(id int) (schema Schema, err error) {
	c.Lock()
	if entry, ok := c.schemaByID.Get(id); ok {
		entry := entry.(*idRegistry)
		for entry.ID == 0 && entry.err == nil {
			//log.Printf("Parking redundant fetch for schema %d while request is in flight.\n", id)
			entry.cond.Wait()
		}
		c.Unlock()
		if entry.err == nil {
			//log.Printf("Returning schema %d from local cache\n", id)
		}
		return entry.Schema, entry.err
	}
	entry := &idRegistry{
		cacheEntry: &cacheEntry{
			cond: sync.NewCond(c),
		},
	}

	c.schemaByID.Add(id, entry)

	defer func() {
		c.Unlock()
		entry.cond.Broadcast()
	}()
	c.Unlock()

	err = c.restService.handleRequest(newRequest("GET", schemas, nil, id), &entry)

	c.Lock()
	entry.err = err
	entry.ID = id

	if err != nil {
		c.schemaByID.Remove(id)
		return entry.Schema, err
	}

	c.schemaByID.Add(id, entry)

	return entry.Schema, err
}

// GetID checks if a schema has been registered with the subject. Returns SchemaDescriptor if the registration can be found
func (c *client) GetID(subject string, schema Schema) (result *Version, err error) {
	sd := &Version{
		Schema: schema,
	}

	err = c.restService.handleRequest(newRequest("POST", subjects, sd, subject), &result)

	return result, err
}

// GetLatestSchemaMetadata fetches latest version registered with the provided subject
// Returns Version object
func (c *client) GetLatestSchemaMetadata(subject string) (result *Version, err error) {
	err = c.restService.handleRequest(newRequest("GET", versions, nil, subject, "latest"), &result)

	return result, err
}

// GetSchemaMetadata fetches the requested subject schema identified by version
// Returns Version object
func (c *client) GetSchemaMetadata(subject string, version int) (result *Version, err error) {
	err = c.restService.handleRequest(newRequest("GET", versions, nil, subject, version), &result)

	return result, err
}

// GetAllVersions fetches a list of all version numbers associated with the provided subject registration
// Returns integer slice on success
func (c *client) GetAllVersions(subject string) (results []int, err error) {
	var result []int
	err = c.restService.handleRequest(newRequest("GET", version, nil, subject), &result)

	return result, err
}

// GetVersion finds the Subject Version associated with the provided schema
// Returns integer Version number
func (c *client) GetVersion(subject string, schema Schema) (id int, err error) {
	result := &Version{}
	result.Schema = schema

	err = c.restService.handleRequest(newRequest("POST", subjects, &result, subject), &result)

	return result.Version, err
}

// Fetch all Subjects registered with the schema Registry
// Returns a string slice containing all registered subjects
func (c *client) GetAllSubjects() ([]string, error) {
	var result []string
	err := c.restService.handleRequest(newRequest("GET", subject, nil), &result)

	return result, err
}

// Deletes provided Subject from registry
// Returns integer slice of versions removed by delete
func (c *client) DeleteSubject(subject string) (deleted []int, err error) {
	var result []int
	err = c.restService.handleRequest(newRequest("DELETE", subjects, nil, subject), &result)

	return result, err
}

// DeleteSubjectVersion removes the version identified by delete from the subject's registration
// Returns integer id for the deleted version
func (c *client) DeleteSubjectVersion(subject string, delete int) (deleted int, err error) {
	var result int
	err = c.restService.handleRequest(newRequest("DELETE", versions, nil, subject, delete), &result)

	return result, err

}

// Compatibility options
type Compatibility int

const (
	_ = iota
	NONE
	BACKWARD
	FORWARD
	FULL
)

var compatibilityENUM = []string{
	"",
	"NONE",
	"BACKWARD",
	"FORWARD",
	"FULL",
}

/* NOTE: GET uses compatibilityLevel, POST uses compatibility */
type compatibilityLevel struct {
	CompatibilityUpdate Compatibility `json:"compatibility,omitempty"`
	Compatibility       Compatibility `json:"compatibilityLevel,omitempty"`
}

// MarshalJSON implements json.Marshaler
func (c Compatibility) MarshalJSON() ([]byte, error) {
	return json.Marshal(compatibilityENUM[c])
}

// UnmarshalJSON implements json.Unmarshaler
func (c *Compatibility) UnmarshalJSON(b []byte) error {
	val := string(b[1 : len(b)-1])
	for idx, elm := range compatibilityENUM {
		if elm == val {
			*c = Compatibility(idx)
			return nil
		}
	}

	return fmt.Errorf("failed to unmarshal Compatibility")
}

type compatibilityValue struct {
	Compatible bool `json:"is_compatible,omitempty"`
}

func (c Compatibility) String() string {
	return compatibilityENUM[c]
}

// Fetch compatibility level currently configured for provided subject
// Returns compatibility level string upon success
func (c *client) GetCompatibility(subject string) (compatibility Compatibility, err error) {
	var result compatibilityLevel
	err = c.restService.handleRequest(newRequest("GET", override, nil, subject), &result)

	return result.Compatibility, err
}

// UpdateCompatibility updates subject's compatibility level
// Returns new compatibility level string upon success
func (c *client) UpdateCompatibility(subject string, update Compatibility) (compatibility Compatibility, err error) {
	result := &compatibilityLevel{
		CompatibilityUpdate: update,
	}
	err = c.restService.handleRequest(newRequest("PUT", override, &result, subject), &result)

	return result.CompatibilityUpdate, err
}

// TestCompatibility verifies schema against the subject's compatibility policy
// Returns true if the schema is compatible, false otherwise
func (c *client) TestCompatibility(subject string, version int, schema Schema) (ok bool, err error) {
	var result compatibilityValue
	candidate := &Version{}
	candidate.Schema = schema

	err = c.restService.handleRequest(newRequest("POST", compatibility, &candidate, subject, version), &result)

	return result.Compatible, err
}

// GetDefaultCompatibility fetches the global(default) compatibility level
// Returns global(default) compatibility level
func (c *client) GetDefaultCompatibility() (compatibility Compatibility, err error) {
	var result compatibilityLevel
	err = c.restService.handleRequest(newRequest("GET", config, nil), &result)

	return result.Compatibility, err
}

// UpdateDefaultCompatibility updates the global(default) compatibility level level
// Returns new string compatibility level
func (c *client) UpdateDefaultCompatibility(update Compatibility) (compatibility Compatibility, err error) {
	result := &compatibilityLevel{
		CompatibilityUpdate: update,
	}
	err = c.restService.handleRequest(newRequest("PUT", config, &result), &result)

	return result.CompatibilityUpdate, err
}
