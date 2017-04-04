/*
Copyright IBM Corp. 2017 All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

                 http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package lib

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"

	"github.com/cloudflare/cfssl/config"
	cfcsr "github.com/cloudflare/cfssl/csr"
	"github.com/cloudflare/cfssl/initca"
	"github.com/cloudflare/cfssl/log"
	"github.com/cloudflare/cfssl/signer"
	"github.com/cloudflare/cfssl/signer/universal"
	"github.com/hyperledger/fabric-ca/api"
	"github.com/hyperledger/fabric-ca/lib/dbutil"
	"github.com/hyperledger/fabric-ca/lib/ldap"
	"github.com/hyperledger/fabric-ca/lib/spi"
	"github.com/hyperledger/fabric-ca/util"
	"github.com/hyperledger/fabric/bccsp"
	"github.com/hyperledger/fabric/bccsp/factory"
	"github.com/jmoiron/sqlx"

	_ "github.com/go-sql-driver/mysql" // import to support MySQL
	_ "github.com/lib/pq"              // import to support Postgres
	_ "github.com/mattn/go-sqlite3"    // import to support SQLite3
)

const (
	defaultDatabaseType = "sqlite3"
	// DefaultCAName is the name for the ca to be used if no ca name provided by client
	DefaultCAName = "ca"
)

// CA represents a certificate authority which signs, issues and revokes certificates
type CA struct {
	// The home directory for the CA
	HomeDir string
	// The CA's configuration
	Config *ServerConfig
	// The parent server URL, which is non-null if this is an intermediate server
	ParentServerURL string
	// The database handle used to store certificates and optionally
	// the user registry information, unless LDAP it enabled for the
	// user registry function.
	db *sqlx.DB
	// The crypto service provider (BCCSP)
	csp bccsp.BCCSP
	// The certificate DB accessor
	certDBAccessor *CertDBAccessor
	// The user registry
	registry spi.UserRegistry
	// The signer used for enrollment
	enrollSigner signer.Signer
}

// NewCA creates a new CA with the specified
// home directory, parent server URL, and config
func NewCA(homeDir, parentServerURL string, config *ServerConfig, renew bool) (*CA, error) {
	ca := &CA{
		HomeDir:         homeDir,
		ParentServerURL: parentServerURL,
		Config:          config,
	}

	err := ca.init(renew)
	if err != nil {
		return nil, err
	}

	return ca, nil
}

// initCA will initialize the passed in pointer to a CA struct
func initCA(ca *CA, homeDir, parentServerURL string, config *ServerConfig, renew bool) error {
	ca.HomeDir = homeDir
	ca.ParentServerURL = parentServerURL
	ca.Config = config

	err := ca.init(renew)
	if err != nil {
		return err
	}

	return nil
}

// Init initializes an instance of a CA
func (ca *CA) init(renew bool) (err error) {
	log.Debug("CA initialization started")
	// Initialize the config, setting defaults, etc
	err = ca.initConfig()
	if err != nil {
		return err
	}
	// Initialize the Crypto Service Provider
	ca.csp = factory.GetDefault()
	// Initialize key materials
	err = ca.initKeyMaterial(renew)
	if err != nil {
		return err
	}
	// Initialize the database
	err = ca.initDB()
	if err != nil {
		return err
	}
	// Initialize the enrollment signer
	err = ca.initEnrollmentSigner()
	if err != nil {
		return err
	}
	log.Debug("CA initialization successful")
	// Successful initialization
	return nil
}

// Initialize the CA's key material
func (ca *CA) initKeyMaterial(renew bool) error {
	log.Debugf("Init CA with home %s and config %+v", ca.HomeDir, *ca.Config)

	// Make the path names absolute in the config
	ca.makeFileNamesAbsolute()

	keyFile := ca.Config.CA.Keyfile
	certFile := ca.Config.CA.Certfile

	// If we aren't renewing and the key and cert files exist, do nothing
	if !renew {
		// If they both exist, the CA was already initialized
		keyFileExists := util.FileExists(keyFile)
		certFileExists := util.FileExists(certFile)
		if keyFileExists && certFileExists {
			log.Info("The CA key and certificate files already exist")
			log.Infof("Key file location: %s", keyFile)
			log.Infof("Certificate file location: %s", certFile)
			return nil
		}
	}

	// Get the CA cert and key
	cert, key, err := ca.getCACertAndKey()
	if err != nil {
		return fmt.Errorf("Failed to initialize CA: %s", err)
	}

	// Store the key and certificate to file
	err = writeFile(keyFile, key, 0600)
	if err != nil {
		return fmt.Errorf("Failed to store key: %s", err)
	}
	err = writeFile(certFile, cert, 0644)
	if err != nil {
		return fmt.Errorf("Failed to store certificate: %s", err)
	}
	log.Info("The CA key and certificate files were generated")
	log.Infof("Key file location: %s", keyFile)
	log.Infof("Certificate file location: %s", certFile)
	return nil
}

// Get the CA certificate and key for this CA
func (ca *CA) getCACertAndKey() (cert, key []byte, err error) {
	log.Debugf("Getting CA cert and key; parent server URL is '%s'", ca.ParentServerURL)
	if ca.ParentServerURL != "" {
		// This is an intermediate CA, so call the parent fabric-ca-server
		// to get the key and cert
		clientCfg := ca.Config.Client
		if clientCfg == nil {
			clientCfg = &ClientConfig{}
		}
		if clientCfg.Enrollment.Profile == "" {
			clientCfg.Enrollment.Profile = "ca"
		}
		if clientCfg.Enrollment.CSR == nil {
			clientCfg.Enrollment.CSR = &api.CSRInfo{}
		}
		if clientCfg.Enrollment.CSR.CA == nil {
			clientCfg.Enrollment.CSR.CA = &cfcsr.CAConfig{PathLength: 0, PathLenZero: true}
		}
		log.Debugf("Intermediate enrollment request: %v", clientCfg.Enrollment)
		var resp *EnrollmentResponse
		resp, err = clientCfg.Enroll(ca.ParentServerURL, ca.HomeDir)
		if err != nil {
			return nil, nil, err
		}
		ecert := resp.Identity.GetECert()
		if ecert == nil {
			return nil, nil, errors.New("No ECert from parent server")
		}
		cert = ecert.Cert()
		key = ecert.Key()
		// Store the chain file as the concatenation of the parent's chain plus the cert.
		chainPath := ca.Config.CA.Chainfile
		if chainPath == "" {
			chainPath, err = util.MakeFileAbs("ca-chain.pem", ca.HomeDir)
			if err != nil {
				return nil, nil, fmt.Errorf("Failed to create intermediate chain file path: %s", err)
			}
		}
		chain := ca.concatChain(resp.ServerInfo.CAChain, cert)
		err = os.MkdirAll(path.Dir(chainPath), 0755)
		if err != nil {
			return nil, nil, fmt.Errorf("Failed to create intermediate chain file directory: %s", err)
		}
		err = util.WriteFile(chainPath, chain, 0644)
		if err != nil {
			return nil, nil, fmt.Errorf("Failed to create intermediate chain file: %s", err)
		}
		log.Debugf("Stored intermediate certificate chain at %s", chainPath)
	} else {
		// This is a root CA, so call cfssl to get the key and cert.
		csr := &ca.Config.CSR
		req := cfcsr.CertificateRequest{
			CN:    csr.CN,
			Names: csr.Names,
			Hosts: csr.Hosts,
			// FIXME: NewBasicKeyRequest only does ecdsa 256; use config
			KeyRequest:   cfcsr.NewBasicKeyRequest(),
			CA:           csr.CA,
			SerialNumber: csr.SerialNumber,
		}
		// Call CFSSL to initialize the CA
		cert, _, key, err = initca.New(&req)
	}
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

// Return a certificate chain which is the concatenation of chain and cert
func (ca *CA) concatChain(chain []byte, cert []byte) []byte {
	result := make([]byte, len(chain)+len(cert))
	copy(result[:len(chain)], chain)
	copy(result[len(chain):], cert)
	return result
}

// Get the certificate chain for the CA
func (ca *CA) getCAChain() (chain []byte, err error) {
	if ca.Config == nil {
		return nil, errors.New("The server has no configuration")
	}
	certAuth := &ca.Config.CA
	// If the chain file exists, we always return the chain from here
	if util.FileExists(certAuth.Chainfile) {
		return util.ReadFile(certAuth.Chainfile)
	}
	// Otherwise, if this is a root CA, we always return the contents of the CACertfile
	if ca.ParentServerURL == "" {
		return util.ReadFile(certAuth.Certfile)
	}
	// If this is an intermediate CA but the ca.Chainfile doesn't exist,
	// it is an error.  It should have been created during intermediate CA enrollment.
	return nil, fmt.Errorf("Chain file does not exist at %s", certAuth.Chainfile)
}

// Initialize the configuration for the CA setting any defaults and making filenames absolute
func (ca *CA) initConfig() (err error) {
	// Init home directory if not set
	if ca.HomeDir == "" {
		ca.HomeDir, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("Failed to initialize CA's home directory: %s", err)
		}
	}
	// Init config if not set
	if ca.Config == nil {
		ca.Config = new(ServerConfig)
	}
	// Set config defaults
	cfg := ca.Config
	if cfg.CA.Certfile == "" {
		cfg.CA.Certfile = "ca-cert.pem"
	}
	if cfg.CA.Keyfile == "" {
		cfg.CA.Keyfile = "ca-key.pem"
	}
	if cfg.CSR.CN == "" {
		cfg.CSR.CN = "fabric-ca-server"
	}
	// Set log level if debug is true
	if cfg.Debug {
		log.Level = log.LevelDebug
	}
	// Init the BCCSP
	err = factory.InitFactories(ca.Config.CSP)
	if err != nil {
		panic(fmt.Errorf("Could not initialize BCCSP Factories %s", err))
	}

	return nil
}

// Initialize the database for the CA
func (ca *CA) initDB() error {
	db := &ca.Config.DB

	var err error
	var exists bool

	if db.Type == "" || db.Type == defaultDatabaseType {

		db.Type = defaultDatabaseType

		if db.Datasource == "" {
			db.Datasource = "fabric-ca-server.db"
		}

		db.Datasource, err = util.MakeFileAbs(db.Datasource, ca.HomeDir)
		if err != nil {
			return err
		}
	}

	log.Debugf("Initializing '%s' database at '%s'", db.Type, db.Datasource)

	switch db.Type {
	case defaultDatabaseType:
		ca.db, exists, err = dbutil.NewUserRegistrySQLLite3(db.Datasource)
		if err != nil {
			return err
		}
	case "postgres":
		ca.db, exists, err = dbutil.NewUserRegistryPostgres(db.Datasource, &db.TLS)
		if err != nil {
			return err
		}
	case "mysql":
		ca.db, exists, err = dbutil.NewUserRegistryMySQL(db.Datasource, &db.TLS)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("Invalid db.type in config file: '%s'; must be 'sqlite3', 'postgres', or 'mysql'", db.Type)
	}

	// Set the certificate DB accessor
	ca.certDBAccessor = NewCertDBAccessor(ca.db)

	// Initialize the user registry.
	// If LDAP is not configured, the fabric-ca CA functions as a user
	// registry based on the database.
	err = ca.initUserRegistry()
	if err != nil {
		return err
	}

	// If the DB doesn't exist, bootstrap it
	if !exists {
		err = ca.loadUsersTable()
		if err != nil {
			return err
		}
		err = ca.loadAffiliationsTable()
		if err != nil {
			return err
		}
	}
	log.Infof("Initialized %s database at %s", db.Type, db.Datasource)
	return nil
}

// Initialize the user registry interface
func (ca *CA) initUserRegistry() error {
	log.Debug("Initializing identity registry")
	var err error
	ldapCfg := &ca.Config.LDAP

	if ldapCfg.Enabled {
		// Use LDAP for the user registry
		ca.registry, err = ldap.NewClient(ldapCfg)
		log.Debugf("Initialized LDAP identity registry; err=%s", err)
		return err
	}

	// Use the DB for the user registry
	dbAccessor := new(Accessor)
	dbAccessor.SetDB(ca.db)
	ca.registry = dbAccessor
	log.Debug("Initialized DB identity registry")
	return nil
}

// Initialize the enrollment signer
func (ca *CA) initEnrollmentSigner() (err error) {
	log.Debug("Initializing enrollment signer")
	c := ca.Config

	// If there is a config, use its signing policy. Otherwise create a default policy.
	var policy *config.Signing
	if c.Signing != nil {
		policy = c.Signing
	} else {
		policy = &config.Signing{
			Profiles: map[string]*config.SigningProfile{},
			Default:  config.DefaultConfig(),
		}
		policy.Default.CAConstraint.IsCA = true
	}

	// Make sure the policy reflects the new remote
	if c.Remote != "" {
		err = policy.OverrideRemotes(c.Remote)
		if err != nil {
			return fmt.Errorf("Failed initializing enrollment signer: %s", err)
		}
	}

	// Get CFSSL's universal root and signer
	root := universal.Root{
		Config: map[string]string{
			"cert-file": c.CA.Certfile,
			"key-file":  c.CA.Keyfile,
		},
		ForceRemote: c.Remote != "",
	}
	ca.enrollSigner, err = universal.NewSigner(root, policy)
	if err != nil {
		return err
	}
	ca.enrollSigner.SetDBAccessor(ca.certDBAccessor)

	// Successful enrollment
	return nil
}

// loadUsersTable adds the configured users to the table if not already found
func (ca *CA) loadUsersTable() error {
	log.Debug("Loading identity table")
	registry := &ca.Config.Registry
	for _, id := range registry.Identities {
		log.Debugf("Loading identity '%s'", id.Name)
		err := ca.addIdentity(&id, false)
		if err != nil {
			return err
		}
	}
	log.Debug("Successfully loaded identity table")
	return nil
}

// loadAffiliationsTable adds the configured affiliations to the table
func (ca *CA) loadAffiliationsTable() error {
	log.Debug("Loading affiliations table")
	err := ca.loadAffiliationsTableR(ca.Config.Affiliations, "")
	if err == nil {
		log.Debug("Successfully loaded affiliations table")
	}
	log.Debug("Successfully loaded groups table")
	return nil
}

// Recursive function to load the affiliations table hierarchy
func (ca *CA) loadAffiliationsTableR(val interface{}, parentPath string) (err error) {
	var path string
	if val == nil {
		return nil
	}
	switch val.(type) {
	case string:
		path = affiliationPath(val.(string), parentPath)
		err = ca.addAffiliation(path, parentPath)
		if err != nil {
			return err
		}
	case []string:
		for _, ele := range val.([]string) {
			err = ca.loadAffiliationsTableR(ele, parentPath)
			if err != nil {
				return err
			}
		}
	case []interface{}:
		for _, ele := range val.([]interface{}) {
			err = ca.loadAffiliationsTableR(ele, parentPath)
			if err != nil {
				return err
			}
		}
	default:
		for name, ele := range val.(map[string]interface{}) {
			path = affiliationPath(name, parentPath)
			err = ca.addAffiliation(path, parentPath)
			if err != nil {
				return err
			}
			err = ca.loadAffiliationsTableR(ele, path)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Add an identity to the registry
func (ca *CA) addIdentity(id *ServerConfigIdentity, errIfFound bool) error {
	var err error
	user, _ := ca.registry.GetUser(id.Name, nil)
	if user != nil {
		if errIfFound {
			return fmt.Errorf("Identity '%s' is already registered", id.Name)
		}
		log.Debugf("Loaded identity: %+v", id)
		return nil
	}

	maxEnrollments, err := ca.getMaxEnrollments(id.MaxEnrollments)
	if err != nil {
		return err
	}
	rec := spi.UserInfo{
		Name:           id.Name,
		Pass:           id.Pass,
		Type:           id.Type,
		Affiliation:    id.Affiliation,
		Attributes:     ca.convertAttrs(id.Attrs),
		MaxEnrollments: maxEnrollments,
	}
	err = ca.registry.InsertUser(rec)
	if err != nil {
		return fmt.Errorf("Failed to insert identity '%s': %s", id.Name, err)
	}
	log.Debugf("Registered identity: %+v", id)
	return nil
}

func (ca *CA) addAffiliation(path, parentPath string) error {
	log.Debugf("Adding affiliation %s", path)
	return ca.registry.InsertAffiliation(path, parentPath)
}

// CertDBAccessor returns the certificate DB accessor for CA
func (ca *CA) CertDBAccessor() *CertDBAccessor {
	return ca.certDBAccessor
}

// DBAccessor returns the registry DB accessor for server
func (ca *CA) DBAccessor() spi.UserRegistry {
	return ca.registry
}

func (ca *CA) convertAttrs(inAttrs map[string]string) []api.Attribute {
	var outAttrs []api.Attribute
	for name, value := range inAttrs {
		outAttrs = append(outAttrs, api.Attribute{
			Name:  name,
			Value: value,
		})
	}
	return outAttrs
}

// Get max enrollments relative to the configured max
func (ca *CA) getMaxEnrollments(requestedMax int) (int, error) {
	configuredMax := ca.Config.Registry.MaxEnrollments
	if requestedMax < 0 {
		return configuredMax, nil
	}
	if configuredMax == 0 {
		// no limit, so grant any request
		return requestedMax, nil
	}
	if requestedMax == 0 && configuredMax != 0 {
		return 0, fmt.Errorf("Infinite enrollments is not permitted; max is %d",
			configuredMax)
	}
	if requestedMax > configuredMax {
		return 0, fmt.Errorf("Max enrollments of %d is not permitted; max is %d",
			requestedMax, configuredMax)
	}
	return requestedMax, nil
}

// Make all file names in the config absolute
func (ca *CA) makeFileNamesAbsolute() error {
	fields := []*string{
		&ca.Config.CA.Certfile,
		&ca.Config.CA.Keyfile,
		&ca.Config.CA.Chainfile,
		&ca.Config.TLS.CertFile,
		&ca.Config.TLS.KeyFile,
	}
	for _, namePtr := range fields {
		abs, err := util.MakeFileAbs(*namePtr, ca.HomeDir)
		if err != nil {
			return err
		}
		*namePtr = abs
	}
	return nil
}

// userHasAttribute returns nil if the user has the attribute, or an
// appropriate error if the user does not have this attribute.
func (ca *CA) userHasAttribute(username, attrname string) error {
	val, err := ca.getUserAttrValue(username, attrname)
	if err != nil {
		return err
	}
	if val == "" {
		return fmt.Errorf("Identity '%s' does not have attribute '%s'", username, attrname)
	}
	return nil
}

// getUserAttrValue returns a user's value for an attribute
func (ca *CA) getUserAttrValue(username, attrname string) (string, error) {
	log.Debugf("getUserAttrValue identity=%s, attr=%s", username, attrname)
	user, err := ca.registry.GetUser(username, []string{attrname})
	if err != nil {
		return "", err
	}
	attrval := user.GetAttribute(attrname)
	log.Debugf("getUserAttrValue identity=%s, name=%s, value=%s", username, attrname, attrval)
	return attrval, nil
}

// getUserAffiliation returns a user's affiliation
func (ca *CA) getUserAffiliation(username string) (string, error) {
	log.Debugf("getUserAffilliation identity=%s", username)
	user, err := ca.registry.GetUserInfo(username)
	if err != nil {
		return "", err
	}
	aff := user.Affiliation
	log.Debugf("getUserAffiliation identity=%s, aff=%s", username, aff)
	return aff, nil
}

// Fill the CA info structure appropriately
func (ca *CA) fillCAInfo(info *serverInfoResponseNet) error {
	caChain, err := ca.getCAChain()
	if err != nil {
		return err
	}
	info.CAName = ca.Config.CA.Name
	info.CAChain = util.B64Encode(caChain)
	return nil
}

func writeFile(file string, buf []byte, perm os.FileMode) error {
	err := os.MkdirAll(filepath.Dir(file), 0755)
	if err != nil {
		return err
	}
	return ioutil.WriteFile(file, buf, perm)
}

func affiliationPath(name, parent string) string {
	if parent == "" {
		return name
	}
	return fmt.Sprintf("%s.%s", parent, name)
}