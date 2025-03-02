// Tyk Gateway API
//
// The code below describes the Tyk Gateway API
// Version: 2.8.0
//
//     Schemes: https, http
//     Host: localhost
//     BasePath: /tyk/
//
//     Consumes:
//     - application/json
//
//     Produces:
//     - application/json
//
//     Security:
//     - api_key:
//
//     SecurityDefinitions:
//     api_key:
//          type: apiKey
//          name: X-Tyk-Authorization
//          in: header
//
// swagger:meta
package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TykTechnologies/tyk/apidef/oas"
	"github.com/getkin/kin-openapi/openapi3"
	uuid "github.com/satori/go.uuid"

	"github.com/gorilla/mux"
	"github.com/lonelycode/osin"

	"github.com/sirupsen/logrus"
	"github.com/spf13/afero"
	"golang.org/x/crypto/bcrypt"

	"github.com/TykTechnologies/tyk/apidef"
	"github.com/TykTechnologies/tyk/ctx"
	"github.com/TykTechnologies/tyk/headers"
	"github.com/TykTechnologies/tyk/storage"
	"github.com/TykTechnologies/tyk/user"

	gql "github.com/jensneuse/graphql-go-tools/pkg/graphql"
)

// apiModifyKeySuccess represents when a Key modification was successful
//
// swagger:model apiModifyKeySuccess
type apiModifyKeySuccess struct {
	// in:body
	Key     string `json:"key"`
	Status  string `json:"status"`
	Action  string `json:"action"`
	KeyHash string `json:"key_hash,omitempty"`
}

// apiStatusMessage represents an API status message
//
// swagger:model apiStatusMessage
type apiStatusMessage struct {
	Status string `json:"status"`
	// Response details
	Message string `json:"message"`
}

func apiOk(msg string) apiStatusMessage {
	return apiStatusMessage{"ok", msg}
}

func apiError(msg string) apiStatusMessage {
	return apiStatusMessage{"error", msg}
}

// paginationStatus provides more information about a paginated data set
type paginationStatus struct {
	PageNum   int `json:"page_num"`
	PageTotal int `json:"page_total"`
	PageSize  int `json:"page_size"`
}

type paginatedOAuthClientTokens struct {
	Pagination paginationStatus
	Tokens     []OAuthClientToken
}

func doJSONWrite(w http.ResponseWriter, code int, obj interface{}) {
	w.Header().Set(headers.ContentType, headers.ApplicationJSON)
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(obj); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
	if code != http.StatusOK {
		job := instrument.NewJob("SystemAPIError")
		job.Event(strconv.Itoa(code))
	}
}

type MethodNotAllowedHandler struct{}

func (m MethodNotAllowedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	doJSONWrite(w, http.StatusMethodNotAllowed, apiError("Method not supported"))
}

func addSecureAndCacheHeaders(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Setting OWASP Secure Headers
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-XSS-Protection", "1; mode=block")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")

		// Avoid Caching of tokens
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		next(w, r)
	}
}

func allowMethods(next http.HandlerFunc, methods ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for _, method := range methods {
			if r.Method == method {
				next(w, r)
				return
			}
		}
		doJSONWrite(w, http.StatusMethodNotAllowed, apiError("Method not supported"))
	}
}

func (gw *Gateway) getSpecForOrg(orgID string) *APISpec {
	gw.apisMu.RLock()
	defer gw.apisMu.RUnlock()
	for _, v := range gw.apisByID {
		if v.OrgID == orgID {
			return v
		}
	}

	// If we can't find a spec, it doesn't matter, because we default to Redis anyway, grab whatever you can find
	for _, v := range gw.apisByID {
		return v
	}
	return nil
}

func (gw *Gateway) getApisIdsForOrg(orgID string) []string {
	result := []string{}

	showAll := orgID == ""
	gw.apisMu.RLock()
	defer gw.apisMu.RUnlock()
	for _, v := range gw.apisByID {
		if v.OrgID == orgID || showAll {
			result = append(result, v.APIID)
		}
	}

	return result
}

func (gw *Gateway) checkAndApplyTrialPeriod(keyName string, newSession *user.SessionState, isHashed bool) {
	// Check the policies to see if we are forcing an expiry on the key
	for _, polID := range newSession.PolicyIDs() {
		gw.policiesMu.RLock()
		policy, ok := gw.policiesByID[polID]
		gw.policiesMu.RUnlock()
		if !ok {
			continue
		}
		// Are we foring an expiry?
		if policy.KeyExpiresIn > 0 {
			// We are, does the key exist?
			_, found := gw.GlobalSessionManager.SessionDetail(newSession.OrgID, keyName, isHashed)
			if !found {
				// this is a new key, lets expire it
				newSession.Expires = time.Now().Unix() + policy.KeyExpiresIn
			}
		}
	}
}

func (gw *Gateway) applyPoliciesAndSave(keyName string, session *user.SessionState, spec *APISpec, isHashed bool) error {
	// use basic middleware to apply policies to key/session (it also saves it)
	mw := BaseMiddleware{
		Spec: spec,
		Gw:   gw,
	}

	if err := mw.ApplyPolicies(session); err != nil {
		return err
	}

	lifetime := session.Lifetime(spec.SessionLifetime, gw.GetConfig().ForceGlobalSessionLifetime, gw.GetConfig().GlobalSessionLifetime)
	if err := gw.GlobalSessionManager.UpdateSession(keyName, session, lifetime, isHashed); err != nil {
		return err
	}

	return nil
}

func resetAPILimits(accessRights map[string]user.AccessDefinition) {
	for apiID := range accessRights {
		// reset API-level limit to nil if it has a zero-value
		if access := accessRights[apiID]; !access.Limit.IsEmpty() && access.Limit == (user.APILimit{}) {
			access.Limit = user.APILimit{}
			accessRights[apiID] = access
		}
	}
}

func (gw *Gateway) doAddOrUpdate(keyName string, newSession *user.SessionState, dontReset bool, isHashed bool) error {
	// field last_updated plays an important role in in-mem rate limiter
	// so update last_updated to current timestamp only if suppress_reset wasn't set to 1
	if !dontReset {
		newSession.LastUpdated = strconv.Itoa(int(time.Now().Unix()))
	}

	if len(newSession.AccessRights) > 0 {
		// reset API-level limit to empty APILimit if any has a zero-value
		resetAPILimits(newSession.AccessRights)
		// We have a specific list of access rules, only add / update those

		for apiId := range newSession.AccessRights {
			apiSpec := gw.getApiSpec(apiId)
			if apiSpec == nil {
				log.WithFields(logrus.Fields{
					"prefix":      "api",
					"key":         keyName,
					"org_id":      newSession.OrgID,
					"api_id":      apiId,
					"user_id":     "system",
					"user_ip":     "--",
					"path":        "--",
					"server_name": "system",
				}).Error("Could not add key for this API ID, API doesn't exist.")
				return errors.New("API must be active to add keys")
			}
			gw.checkAndApplyTrialPeriod(keyName, newSession, isHashed)

			// Lets reset keys if they are edited by admin
			if !apiSpec.DontSetQuotasOnCreate {
				// Reset quote by default
				if !dontReset {
					gw.GlobalSessionManager.ResetQuota(keyName, newSession, isHashed)
					newSession.QuotaRenews = time.Now().Unix() + newSession.QuotaRenewalRate
				}

				// apply polices (if any) and save key
				if err := gw.applyPoliciesAndSave(keyName, newSession, apiSpec, isHashed); err != nil {
					return err
				}
			}
		}
	} else {
		// nothing defined, add key to ALL
		if !gw.GetConfig().AllowMasterKeys {
			log.Error("Master keys disallowed in configuration, key not added.")
			return errors.New("Master keys not allowed")
		}
		log.Warning("No API Access Rights set, adding key to ALL.")
		gw.apisMu.RLock()
		defer gw.apisMu.RUnlock()
		for _, spec := range gw.apisByID {
			if !dontReset {
				gw.GlobalSessionManager.ResetQuota(keyName, newSession, isHashed)
				newSession.QuotaRenews = time.Now().Unix() + newSession.QuotaRenewalRate
			}
			gw.checkAndApplyTrialPeriod(keyName, newSession, isHashed)

			// apply polices (if any) and save key
			if err := gw.applyPoliciesAndSave(keyName, newSession, spec, isHashed); err != nil {
				return err
			}
		}
	}

	log.WithFields(logrus.Fields{
		"prefix":      "api",
		"key":         gw.obfuscateKey(keyName),
		"expires":     newSession.Expires,
		"org_id":      newSession.OrgID,
		"api_id":      "--",
		"user_id":     "system",
		"user_ip":     "--",
		"path":        "--",
		"server_name": "system",
	}).Info("Key added or updated.")
	return nil
}

// ---- TODO: This changes the URL structure of the API completely ----
// ISSUE: If Session stores are stored with API specs, then managing keys will need to be done per store, i.e. add to all stores,
// remove from all stores, update to all stores, stores handle quotas separately though because they are localised! Keys will
// need to be managed by API, but only for GetDetail, GetList, UpdateKey and DeleteKey

func setSessionPassword(session *user.SessionState) {
	session.BasicAuthData.Hash = user.HashBCrypt
	newPass, err := bcrypt.GenerateFromPassword([]byte(session.BasicAuthData.Password), 10)
	if err != nil {
		log.Error("Could not hash password, setting to plaintext, error was: ", err)
		session.BasicAuthData.Hash = user.HashPlainText
		return
	}

	session.BasicAuthData.Password = string(newPass)
}

func (gw *Gateway) handleAddOrUpdate(keyName string, r *http.Request, isHashed bool) (interface{}, int) {
	suppressReset := r.URL.Query().Get("suppress_reset") == "1"

	// decode payload
	newSession := &user.SessionState{}

	contents, _ := ioutil.ReadAll(r.Body)
	r.Body = ioutil.NopCloser(bytes.NewReader(contents))

	if err := json.Unmarshal(contents, newSession); err != nil {
		log.Error("Couldn't decode new session object: ", err)
		return apiError("Request malformed"), http.StatusBadRequest
	}

	mw := BaseMiddleware{Gw: gw}
	// TODO: handle apply policies error
	mw.ApplyPolicies(newSession)
	// DO ADD OR UPDATE

	// get original session in case of update and preserve fields that SHOULD NOT be updated
	originalKey := user.SessionState{}
	if r.Method == http.MethodPut {
		key, found := gw.GlobalSessionManager.SessionDetail(newSession.OrgID, keyName, isHashed)
		keyName = key.KeyID
		if !found {
			log.Error("Could not find key when updating")
			return apiError("Key is not found"), http.StatusNotFound
		}
		originalKey = key.Clone()

		isCertificateChanged := newSession.Certificate != originalKey.Certificate
		if isCertificateChanged {
			if newSession.Certificate == "" {
				log.Error("Key must contain a certificate")
				return apiError("Key cannot be used without a certificate"), http.StatusBadRequest
			}

			// check that the certificate exists in the system
			_, err := gw.CertificateManager.GetRaw(newSession.Certificate)
			if err != nil {
				log.Error("Key must contain an existing certificate")
				return apiError("Key must be used with an existent certificate"), http.StatusBadRequest
			}
		}

		// preserve the creation date
		newSession.DateCreated = originalKey.DateCreated

		// don't change fields related to quota and rate limiting if was passed as "suppress_reset=1"
		if suppressReset {
			// save existing quota_renews and last_updated if suppress_reset was passed
			// (which means don't reset quota or rate counters)
			// - leaving quota_renews as 0 will force quota limiter to start new renewal period
			// - setting new last_updated with force rate limiter to start new "per" rating period

			// on session level
			newSession.QuotaRenews = originalKey.QuotaRenews
			newSession.LastUpdated = originalKey.LastUpdated

			// on ACL API limit level
			for apiID, access := range originalKey.AccessRights {
				if access.Limit.IsEmpty() {
					continue
				}
				if newAccess, ok := newSession.AccessRights[apiID]; ok && !newAccess.Limit.IsEmpty() {
					newAccess.Limit.QuotaRenews = access.Limit.QuotaRenews
					newSession.AccessRights[apiID] = newAccess
				}
			}
		}
	} else {
		newSession.DateCreated = time.Now()
		keyName = gw.generateToken(newSession.OrgID, keyName)
	}

	//set the original expiry if the content in payload is a past time
	if time.Now().After(time.Unix(newSession.Expires, 0)) && newSession.Expires > 1 {
		newSession.Expires = originalKey.Expires
	}

	// Update our session object (create it)
	if newSession.BasicAuthData.Password != "" {
		// If we are using a basic auth user, then we need to make the keyname explicit against the OrgId in order to differentiate it
		// Only if it's NEW
		switch r.Method {
		case http.MethodPost:
			// It's a create, so lets hash the password
			setSessionPassword(newSession)
		case http.MethodPut:
			if originalKey.BasicAuthData.Password != newSession.BasicAuthData.Password {
				// passwords dont match assume it's new, lets hash it
				log.Debug("Passwords dont match, original: ", originalKey.BasicAuthData.Password)
				log.Debug("New: newSession.BasicAuthData.Password")
				log.Debug("Changing password")
				setSessionPassword(newSession)
			}
		}
	} else if originalKey.BasicAuthData.Password != "" {
		// preserve basic auth data
		newSession.BasicAuthData.Hash = originalKey.BasicAuthData.Hash
		newSession.BasicAuthData.Password = originalKey.BasicAuthData.Password
	}

	if r.Method == http.MethodPost || storage.TokenOrg(keyName) != "" {
		// use new key format if key gets created or updating key with new format
		if err := gw.doAddOrUpdate(keyName, newSession, suppressReset, isHashed); err != nil {
			return apiError("Failed to create key, ensure security settings are correct."), http.StatusInternalServerError
		}
	} else {

		newFormatKey := gw.generateToken(newSession.OrgID, keyName)
		// search as a custom key
		_, err := gw.GlobalSessionManager.Store().GetKey(newFormatKey)

		if err == nil {
			// update new format key for custom keys, as it was found then its a customKey
			keyName = newFormatKey
		}

		if err := gw.doAddOrUpdate(keyName, newSession, suppressReset, isHashed); err != nil {
			return apiError("Failed to create key, ensure security settings are correct."), http.StatusInternalServerError
		}
	}

	action := "modified"
	event := EventTokenUpdated
	if r.Method == http.MethodPost {
		action = "added"
		event = EventTokenCreated
	}
	gw.FireSystemEvent(event, EventTokenMeta{
		EventMetaDefault: EventMetaDefault{Message: "Key modified."},
		Org:              newSession.OrgID,
		Key:              keyName,
	})

	response := apiModifyKeySuccess{
		Key:    keyName,
		Status: "ok",
		Action: action,
	}

	// add key hash for newly created key
	if gw.GetConfig().HashKeys && r.Method == http.MethodPost {
		if isHashed {
			response.KeyHash = keyName
		} else {
			response.KeyHash = storage.HashKey(keyName, gw.GetConfig().HashKeys)
		}
	}

	return response, http.StatusOK
}

func (gw *Gateway) handleGetDetail(sessionKey, apiID, orgID string, byHash bool) (interface{}, int) {
	if byHash && !gw.GetConfig().HashKeys {
		return apiError("Key requested by hash but key hashing is not enabled"), http.StatusBadRequest
	}

	spec := gw.getApiSpec(apiID)
	if spec != nil {
		orgID = spec.OrgID
	}

	session, ok := gw.GlobalSessionManager.SessionDetail(orgID, sessionKey, byHash)
	sessionKey = session.KeyID
	if !ok {
		return apiError("Key not found"), http.StatusNotFound
	}

	mw := BaseMiddleware{Spec: spec, Gw: gw}
	// TODO: handle apply policies error
	mw.ApplyPolicies(&session)

	if session.QuotaMax != -1 {
		quotaKey := QuotaKeyPrefix + storage.HashKey(sessionKey, gw.GetConfig().HashKeys)
		if byHash {
			quotaKey = QuotaKeyPrefix + sessionKey
		}

		if usedQuota, err := gw.GlobalSessionManager.Store().GetRawKey(quotaKey); err == nil {
			qInt, _ := strconv.Atoi(usedQuota)
			remaining := session.QuotaMax - int64(qInt)

			if remaining < 0 {
				session.QuotaRemaining = 0
			} else {
				session.QuotaRemaining = remaining
			}
		} else {
			log.WithFields(logrus.Fields{
				"prefix":  "api",
				"key":     gw.obfuscateKey(quotaKey),
				"message": err,
				"status":  "ok",
			}).Info("Can't retrieve key quota")
		}
	}

	// populate remaining quota for API limits (if any)
	for id, access := range session.AccessRights {
		if access.Limit.IsEmpty() || access.Limit.QuotaMax == -1 || access.Limit.QuotaMax == 0 {
			continue
		}

		quotaScope := ""
		if access.AllowanceScope != "" {
			quotaScope = access.AllowanceScope + "-"
		}

		limQuotaKey := QuotaKeyPrefix + quotaScope + storage.HashKey(sessionKey, gw.GetConfig().HashKeys)
		if byHash {
			limQuotaKey = QuotaKeyPrefix + quotaScope + sessionKey
		}

		if usedQuota, err := gw.GlobalSessionManager.Store().GetRawKey(limQuotaKey); err == nil {
			qInt, _ := strconv.Atoi(usedQuota)
			remaining := access.Limit.QuotaMax - int64(qInt)

			if remaining < 0 {
				access.Limit.QuotaRemaining = 0
			} else {
				access.Limit.QuotaRemaining = remaining
			}
			session.AccessRights[id] = access
		} else {
			access.Limit.QuotaRemaining = access.Limit.QuotaMax
			session.AccessRights[id] = access

			log.WithFields(logrus.Fields{
				"prefix": "api",
				"apiID":  id,
				"key":    gw.obfuscateKey(sessionKey),
				"error":  err,
			}).Info("Can't retrieve api limit quota")
		}
	}

	// If it's a basic auth key and a valid Base64 string, use it as the key ID:
	if session.BasicAuthData.Password != "" {
		if storage.TokenOrg(sessionKey) != "" {
			session.KeyID = sessionKey
		}
	}

	log.WithFields(logrus.Fields{
		"prefix": "api",
		"key":    gw.obfuscateKey(sessionKey),
		"status": "ok",
	}).Info("Retrieved key detail.")

	return session.Clone(), http.StatusOK
}

// apiAllKeys represents a list of keys in the memory store
// swagger:model
type apiAllKeys struct {
	APIKeys []string `json:"keys"`
}

func (gw *Gateway) handleGetAllKeys(filter string) (interface{}, int) {
	sessions := gw.GlobalSessionManager.Sessions(filter)
	if filter != "" {
		filterB64 := base64.StdEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(fmt.Sprintf(`{"org":"%s"`, filter)))
		// Remove last 2 digits to look exact match
		filterB64 = filterB64[0 : len(filterB64)-2]
		orgIDB64Sessions := gw.GlobalSessionManager.Sessions(filterB64)
		sessions = append(sessions, orgIDB64Sessions...)
	}

	fixedSessions := make([]string, 0)
	for _, s := range sessions {
		if !strings.HasPrefix(s, QuotaKeyPrefix) && !strings.HasPrefix(s, RateLimitKeyPrefix) {
			fixedSessions = append(fixedSessions, s)
		}
	}

	sessionsObj := apiAllKeys{fixedSessions}

	log.WithFields(logrus.Fields{
		"prefix": "api",
		"status": "ok",
	}).Info("Retrieved key list.")

	return sessionsObj, http.StatusOK
}

func (gw *Gateway) handleAddKey(keyName, hashedName, sessionString, apiID string, orgId string) {
	sess := &user.SessionState{}
	json.Unmarshal([]byte(sessionString), sess)
	sess.LastUpdated = strconv.Itoa(int(time.Now().Unix()))

	if sess.OrgID != orgId {
		return
	}

	var err error
	if gw.GetConfig().HashKeys {
		err = gw.GlobalSessionManager.UpdateSession(hashedName, sess, 0, true)
	} else {
		err = gw.GlobalSessionManager.UpdateSession(keyName, sess, 0, false)
	}
	if err != nil {
		log.WithFields(logrus.Fields{
			"prefix": "api",
			"key":    gw.obfuscateKey(keyName),
			"status": "fail",
			"err":    err,
		}).Error("Failed to update key.")
	}
	log.WithFields(logrus.Fields{
		"prefix": "RPC",
		"key":    gw.obfuscateKey(keyName),
		"status": "ok",
	}).Info("Updated hashed key in slave storage.")
}

func (gw *Gateway) handleDeleteKey(keyName, orgID, apiID string, resetQuota bool) (interface{}, int) {
	session, ok := gw.GlobalSessionManager.SessionDetail(orgID, keyName, false)
	if !ok {
		return apiError("There is no such key found"), http.StatusNotFound
	}
	keyName = session.KeyID

	if apiID == "-1" {
		// Go through ALL managed API's and delete the key
		gw.apisMu.RLock()
		removed := gw.GlobalSessionManager.RemoveSession(orgID, keyName, false)
		gw.GlobalSessionManager.ResetQuota(keyName, &session, false)
		gw.apisMu.RUnlock()

		if !removed {
			log.WithFields(logrus.Fields{
				"prefix": "api",
				"key":    gw.obfuscateKey(keyName),
				"status": "fail",
			}).Error("Failed to remove the key")
			return apiError("Failed to remove the key"), http.StatusBadRequest
		}

		log.WithFields(logrus.Fields{
			"prefix": "api",
			"key":    keyName,
			"status": "ok",
		}).Info("Deleted key across all APIs.")

		return nil, http.StatusOK
	}

	if !gw.GlobalSessionManager.RemoveSession(orgID, keyName, false) {
		log.WithFields(logrus.Fields{
			"prefix": "api",
			"key":    gw.obfuscateKey(keyName),
			"status": "fail",
		}).Error("Failed to remove the key")
		return apiError("Failed to remove the key"), http.StatusBadRequest
	}

	if resetQuota {
		gw.GlobalSessionManager.ResetQuota(keyName, &session, false)
	}

	statusObj := apiModifyKeySuccess{
		Key:    keyName,
		Status: "ok",
		Action: "deleted",
	}

	gw.FireSystemEvent(EventTokenDeleted, EventTokenMeta{
		EventMetaDefault: EventMetaDefault{Message: "Key deleted."},
		Org:              orgID,
		Key:              keyName,
	})

	log.WithFields(logrus.Fields{
		"prefix": "api",
		"key":    keyName,
		"status": "ok",
	}).Info("Deleted key.")

	return statusObj, http.StatusOK
}

// handleDeleteHashedKeyWithLogs is a wrapper for handleDeleteHashedKey with logs
func (gw *Gateway) handleDeleteHashedKeyWithLogs(keyName, orgID, apiID string, resetQuota bool) (interface{}, int) {
	res, code := gw.handleDeleteHashedKey(keyName, orgID, apiID, resetQuota)

	if code != http.StatusOK {
		log.WithFields(logrus.Fields{
			"prefix": "api",
			"key":    gw.obfuscateKey(keyName),
			"status": "fail",
		}).Error(res)
	}

	log.WithFields(logrus.Fields{
		"prefix": "api",
		"key":    keyName,
		"status": "ok",
	}).Info("Deleted hashed key across all APIs.")

	return res, code
}

func (gw *Gateway) handleDeleteHashedKey(keyName, orgID, apiID string, resetQuota bool) (interface{}, int) {

	session, ok := gw.GlobalSessionManager.SessionDetail(orgID, keyName, true)
	keyName = session.KeyID
	if !ok {
		return apiError("There is no such key found"), http.StatusNotFound

	}

	if apiID == "-1" {
		// Go through ALL managed API's and delete the key
		gw.apisMu.RLock()
		removed := gw.GlobalSessionManager.RemoveSession(orgID, keyName, true)
		gw.apisMu.RUnlock()

		if !removed {
			return apiError("Failed to remove the key"), http.StatusBadRequest
		}

		return nil, http.StatusOK
	}

	if !gw.GlobalSessionManager.RemoveSession(orgID, keyName, true) {
		return apiError("Failed to remove the key"), http.StatusBadRequest
	}

	if resetQuota {
		gw.GlobalSessionManager.ResetQuota(keyName, &session, true)
	}

	statusObj := apiModifyKeySuccess{
		Key:    keyName,
		Status: "ok",
		Action: "deleted",
	}

	return statusObj, http.StatusOK
}

func (gw *Gateway) handleGlobalAddToSortedSet(keyName, value string, score float64) {
	gw.GlobalSessionManager.Store().AddToSortedSet(keyName, value, score)
}

func (gw *Gateway) handleGetSortedSetRange(keyName, scoreFrom, scoreTo string) ([]string, []float64, error) {
	return gw.GlobalSessionManager.Store().GetSortedSetRange(keyName, scoreFrom, scoreTo)
}

func (gw *Gateway) handleRemoveSortedSetRange(keyName, scoreFrom, scoreTo string) error {
	return gw.GlobalSessionManager.Store().RemoveSortedSetRange(keyName, scoreFrom, scoreTo)
}

func (gw *Gateway) handleGetPolicy(polID string) (interface{}, int) {
	if pol := gw.getPolicy(polID); pol.ID != "" {
		return pol, http.StatusOK
	}

	log.WithFields(logrus.Fields{
		"prefix": "policy",
		"polID":  polID,
	}).Error("Policy doesn't exist.")
	return apiError("Policy not found"), http.StatusNotFound
}

func (gw *Gateway) handleGetPolicyList() (interface{}, int) {
	gw.policiesMu.RLock()
	defer gw.policiesMu.RUnlock()
	polIDList := make([]user.Policy, len(gw.policiesByID))
	c := 0
	for _, pol := range gw.policiesByID {
		polIDList[c] = pol
		c++
	}
	return polIDList, http.StatusOK
}

func (gw *Gateway) handleAddOrUpdatePolicy(polID string, r *http.Request) (interface{}, int) {
	if gw.GetConfig().Policies.PolicySource == "service" {
		log.Error("Rejected new policy due to PolicySource = service")
		return apiError("Due to enabled service policy source, please use the Dashboard API"), http.StatusInternalServerError
	}

	newPol := &user.Policy{}
	if err := json.NewDecoder(r.Body).Decode(newPol); err != nil {
		log.Error("Couldn't decode new policy object: ", err)
		return apiError("Request malformed"), http.StatusBadRequest
	}

	if polID != "" && newPol.ID != polID && r.Method == http.MethodPut {
		log.Error("PUT operation on different IDs")
		return apiError("Request ID does not match that in policy! For Update operations these must match."), http.StatusBadRequest
	}

	// Create a filename
	polFilePath := filepath.Join(gw.GetConfig().Policies.PolicyPath, newPol.ID+".json")

	asByte, err := json.MarshalIndent(newPol, "", "  ")
	if err != nil {
		log.Error("Marshalling of policy failed: ", err)
		return apiError("Marshalling failed"), http.StatusInternalServerError
	}

	if err := ioutil.WriteFile(polFilePath, asByte, 0644); err != nil {
		log.Error("Failed to create file! - ", err)
		return apiError("Failed to create file!"), http.StatusInternalServerError
	}

	action := "modified"
	if r.Method == http.MethodPost {
		action = "added"
	}

	response := apiModifyKeySuccess{
		Key:    newPol.ID,
		Status: "ok",
		Action: action,
	}

	return response, http.StatusOK
}

func (gw *Gateway) handleDeletePolicy(polID string) (interface{}, int) {
	// Generate a filename
	defFilePath := filepath.Join(gw.GetConfig().Policies.PolicyPath, polID+".json")

	// If it exists, delete it
	if _, err := os.Stat(defFilePath); err != nil {
		log.Warningf("Error describing named file: %v ", err)
		return apiError("Delete failed"), http.StatusInternalServerError
	}

	if err := os.Remove(defFilePath); err != nil {
		log.Warningf("Delete failed: %v", err)
		return apiError("Delete failed"), http.StatusInternalServerError
	}

	response := apiModifyKeySuccess{
		Key:    polID,
		Status: "ok",
		Action: "deleted",
	}

	return response, http.StatusOK
}

func (gw *Gateway) handleGetAPIList() (interface{}, int) {
	gw.apisMu.RLock()
	defer gw.apisMu.RUnlock()
	apiIDList := make([]*apidef.APIDefinition, len(gw.apisByID))
	c := 0
	for _, apiSpec := range gw.apisByID {
		apiIDList[c] = apiSpec.APIDefinition
		c++
	}
	return apiIDList, http.StatusOK
}

func (gw *Gateway) handleGetAPI(apiID string, oasTyped bool) (interface{}, int) {
	if spec := gw.getApiSpec(apiID); spec != nil {
		if oasTyped {
			return &spec.OAS, http.StatusOK
		} else {
			return spec.APIDefinition, http.StatusOK
		}
	}

	log.WithFields(logrus.Fields{
		"prefix": "api",
		"apiID":  apiID,
	}).Error("API doesn't exist.")
	return apiError("API not found"), http.StatusNotFound
}

func (gw *Gateway) handleAddOrUpdateApi(apiID string, r *http.Request, fs afero.Fs, oasTyped bool) (interface{}, int) {
	if gw.GetConfig().UseDBAppConfigs {
		log.Error("Rejected new API Definition due to UseDBAppConfigs = true")
		return apiError("Due to enabled use_db_app_configs, please use the Dashboard API"), http.StatusInternalServerError
	}

	var newDef apidef.APIDefinition
	var oasDoc openapi3.Swagger
	var xTykAPIGateway oas.XTykAPIGateway

	if oasTyped {
		if err := json.NewDecoder(r.Body).Decode(&oasDoc); err != nil {
			log.Error("Couldn't decode new OAS object: ", err)
			return apiError("Request malformed"), http.StatusBadRequest
		}

		if intTykAPIGateway, ok := oasDoc.Extensions[oas.ExtensionTykAPIGateway]; ok {
			rawTykAPIGateway := intTykAPIGateway.(json.RawMessage)
			err := json.Unmarshal(rawTykAPIGateway, &xTykAPIGateway)
			if err != nil {
				return apiError("Couldn't unmarshal " + oas.ExtensionTykAPIGateway + " extension in the document"), http.StatusBadRequest
			}
		} else {
			return apiError("Couldn't find " + oas.ExtensionTykAPIGateway + " extension in the document"), http.StatusBadRequest
		}

		xTykAPIGateway.ExtractTo(&newDef)
	} else {
		if err := json.NewDecoder(r.Body).Decode(&newDef); err != nil {
			log.Error("Couldn't decode new API Definition object: ", err)
			return apiError("Request malformed"), http.StatusBadRequest
		}

		xTykAPIGateway.Fill(newDef)

		spec := gw.getApiSpec(newDef.APIID)
		if spec != nil {
			oasDoc = spec.OAS
		}

		if oasDoc.Extensions == nil {
			oasDoc.Extensions = make(map[string]interface{})
		}

		oasDoc.Extensions[oas.ExtensionTykAPIGateway] = xTykAPIGateway
	}

	if apiID != "" && newDef.APIID != apiID {
		log.Error("PUT operation on different APIIDs")
		return apiError("Request APIID does not match that in Definition! For Update operations these must match."), http.StatusBadRequest
	}

	validationResult := apidef.Validate(&newDef, apidef.DefaultValidationRuleSet)
	if !validationResult.IsValid {
		reason := "unknown"
		if validationResult.ErrorCount() > 0 {
			reason = validationResult.FirstError().Error()
		}

		log.Debugf("Semantic validation for API Definition failed. Reason: %s.", reason)
		return apiError(fmt.Sprintf("Validation of API Definition failed. Reason: %s.", reason)), http.StatusBadRequest
	}

	// api
	err, errCode := gw.writeToFile(fs, newDef, newDef.APIID)
	if err != nil {
		return apiError(err.Error()), errCode
	}

	// oas
	err, errCode = gw.writeToFile(fs, &oasDoc, newDef.APIID+"-oas")
	if err != nil {
		return apiError(err.Error()), errCode
	}

	action := "modified"
	if r.Method == http.MethodPost {
		action = "added"
	}

	response := apiModifyKeySuccess{
		Key:    newDef.APIID,
		Status: "ok",
		Action: action,
	}

	return response, http.StatusOK
}

func (gw *Gateway) writeToFile(fs afero.Fs, newDef interface{}, filename string) (err error, errCode int) {
	// Create a filename
	defFilePath := filepath.Join(gw.GetConfig().AppPath, filename+".json")

	log.Infof("App path: %v", gw.GetConfig().AppPath)
	// If it exists, delete it
	if _, err := fs.Stat(defFilePath); err == nil {
		log.Warning("API Definition with this ID already exists, deleting file...")
		fs.Remove(defFilePath)
	}

	// unmarshal the object into the file
	asByte, err := json.MarshalIndent(newDef, "", "  ")
	if err != nil {
		log.Error("Marshalling of API Definition failed: ", err)
		return errors.New("marshalling failed"), http.StatusInternalServerError
	}

	if err := ioutil.WriteFile(defFilePath, asByte, 0644); err != nil {
		log.Infof("EL file path: %v", defFilePath)
		log.Error("Failed to create file! - ", err)
		return errors.New("file object creation failed, write error"), http.StatusInternalServerError
	}

	return nil, 0
}

func (gw *Gateway) handleDeleteAPI(apiID string) (interface{}, int) {
	// Generate a filename
	defFilePath := filepath.Join(gw.GetConfig().AppPath, apiID+".json")
	defOASFilePath := filepath.Join(gw.GetConfig().AppPath, apiID+"-oas.json")

	// If it exists, delete it
	if _, err := os.Stat(defFilePath); err != nil {
		log.Warning("File does not exist! ", err)
		return apiError("Delete failed"), http.StatusInternalServerError
	}

	os.Remove(defFilePath)
	os.Remove(defOASFilePath)

	response := apiModifyKeySuccess{
		Key:    apiID,
		Status: "ok",
		Action: "deleted",
	}

	return response, http.StatusOK
}

func (gw *Gateway) polHandler(w http.ResponseWriter, r *http.Request) {
	polID := mux.Vars(r)["polID"]

	var obj interface{}
	var code int

	switch r.Method {
	case http.MethodGet:
		if polID != "" {
			log.Debug("Requesting policy for", polID)
			obj, code = gw.handleGetPolicy(polID)
		} else {
			log.Debug("Requesting Policy list")
			obj, code = gw.handleGetPolicyList()
		}
	case http.MethodPost:
		log.Debug("Creating new definition file")
		obj, code = gw.handleAddOrUpdatePolicy(polID, r)
	case http.MethodPut:
		if polID != "" {
			log.Debug("Updating existing Policy: ", polID)
			obj, code = gw.handleAddOrUpdatePolicy(polID, r)
		} else {
			obj, code = apiError("Must specify an apiID to update"), http.StatusBadRequest
		}
	case http.MethodDelete:
		if polID != "" {
			log.Debug("Deleting policy for: ", polID)
			obj, code = gw.handleDeletePolicy(polID)
		} else {
			obj, code = apiError("Must specify an apiID to delete"), http.StatusBadRequest
		}
	}

	doJSONWrite(w, code, obj)
}

func (gw *Gateway) apiHandler(w http.ResponseWriter, r *http.Request) {
	apiID := mux.Vars(r)["apiID"]
	oasTyped := r.FormValue("type") == "oas"

	var obj interface{}
	var code int

	switch r.Method {
	case "GET":
		if apiID != "" {
			log.Debug("Requesting API definition for", apiID)
			obj, code = gw.handleGetAPI(apiID, oasTyped)
		} else {
			log.Debug("Requesting API list")
			obj, code = gw.handleGetAPIList()
		}
	case "POST":
		log.Debug("Creating new definition file")
		obj, code = gw.handleAddOrUpdateApi(apiID, r, afero.NewOsFs(), oasTyped)
	case "PUT":
		if apiID != "" {
			log.Debug("Updating existing API: ", apiID)
			obj, code = gw.handleAddOrUpdateApi(apiID, r, afero.NewOsFs(), oasTyped)
		} else {
			obj, code = apiError("Must specify an apiID to update"), http.StatusBadRequest
		}
	case "DELETE":
		if apiID != "" {
			log.Debug("Deleting API definition for: ", apiID)
			obj, code = gw.handleDeleteAPI(apiID)
		} else {
			obj, code = apiError("Must specify an apiID to delete"), http.StatusBadRequest
		}
	}

	doJSONWrite(w, code, obj)
}

func (gw *Gateway) keyHandler(w http.ResponseWriter, r *http.Request) {
	keyName := mux.Vars(r)["keyName"]
	apiID := r.URL.Query().Get("api_id")
	isHashed := r.URL.Query().Get("hashed") != ""
	isUserName := r.URL.Query().Get("username") == "true"
	orgID := r.URL.Query().Get("org_id")

	// check if passed key is user name and convert it to real key with respect to current hashing algorithm
	origKeyName := keyName
	if r.Method != http.MethodPost && isUserName {
		keyName = gw.generateToken(orgID, keyName)
	}

	var obj interface{}
	var code int
	gwConfig := gw.GetConfig()
	hashKeyFunction := gwConfig.HashKeyFunction

	switch r.Method {
	case http.MethodPost:
		obj, code = gw.handleAddOrUpdate(keyName, r, isHashed)
	case http.MethodPut:
		obj, code = gw.handleAddOrUpdate(keyName, r, isHashed)
		if code != http.StatusOK && hashKeyFunction != "" {
			// try to use legacy key format
			obj, code = gw.handleAddOrUpdate(origKeyName, r, isHashed)
		}
	case http.MethodGet:
		if keyName != "" {
			// Return single key detail
			obj, code = gw.handleGetDetail(keyName, apiID, orgID, isHashed)
			if code != http.StatusOK && hashKeyFunction != "" {
				// try to use legacy key format
				obj, code = gw.handleGetDetail(origKeyName, apiID, orgID, isHashed)
			}
		} else {
			// Return list of keys
			if gwConfig.HashKeys {
				// get all keys is disabled by default
				if !gwConfig.EnableHashedKeysListing {
					doJSONWrite(
						w,
						http.StatusNotFound,
						apiError("Hashed key listing is disabled in config (enable_hashed_keys_listing)"),
					)
					return
				}

				// we don't use filter for hashed keys
				obj, code = gw.handleGetAllKeys("")
			} else {
				filter := r.URL.Query().Get("filter")
				obj, code = gw.handleGetAllKeys(filter)
			}
		}

	case http.MethodDelete:
		// Remove a key
		if !isHashed {
			obj, code = gw.handleDeleteKey(keyName, orgID, apiID, true)
		} else {
			obj, code = gw.handleDeleteHashedKeyWithLogs(keyName, orgID, apiID, true)
		}
		if code != http.StatusOK && hashKeyFunction != "" {
			// try to use legacy key format
			if !isHashed {
				obj, code = gw.handleDeleteKey(origKeyName, orgID, apiID, true)
			} else {
				obj, code = gw.handleDeleteHashedKeyWithLogs(origKeyName, orgID, apiID, true)
			}
		}
	}

	doJSONWrite(w, code, obj)
}

type PolicyUpdateObj struct {
	Policy        string   `json:"policy"`
	ApplyPolicies []string `json:"apply_policies"`
}

func (gw *Gateway) policyUpdateHandler(w http.ResponseWriter, r *http.Request) {
	log.Warning("Hashed key change request detected!")

	var policRecord PolicyUpdateObj
	if err := json.NewDecoder(r.Body).Decode(&policRecord); err != nil {
		doJSONWrite(w, http.StatusBadRequest, apiError("Couldn't decode instruction"))
		return
	}

	if policRecord.Policy != "" {
		policRecord.ApplyPolicies = append(policRecord.ApplyPolicies, policRecord.Policy)
	}

	keyName := mux.Vars(r)["keyName"]
	obj, code := gw.handleUpdateHashedKey(keyName, policRecord.ApplyPolicies)

	doJSONWrite(w, code, obj)
}

func (gw *Gateway) handleUpdateHashedKey(keyName string, applyPolicies []string) (interface{}, int) {
	var orgID string
	if len(applyPolicies) != 0 {
		gw.policiesMu.RLock()
		orgID = gw.policiesByID[applyPolicies[0]].OrgID
		gw.policiesMu.RUnlock()
	}

	sess, ok := gw.GlobalSessionManager.SessionDetail(orgID, keyName, true)
	keyName = sess.KeyID
	if !ok {
		log.WithFields(logrus.Fields{
			"prefix": "api",
			"key":    keyName,
			"status": "fail",
		}).Error("Failed to update hashed key.")

		return apiError("Key not found"), http.StatusNotFound
	}

	// Set the policy
	sess.LastUpdated = strconv.Itoa(int(time.Now().Unix()))
	sess.SetPolicies(applyPolicies...)

	err := gw.GlobalSessionManager.UpdateSession(keyName, &sess, 0, true)
	if err != nil {
		log.WithFields(logrus.Fields{
			"prefix": "api",
			"key":    keyName,
			"status": "fail",
			"err":    err,
		}).Error("Failed to update hashed key.")

		return apiError("Could not write key data"), http.StatusInternalServerError
	}

	statusObj := apiModifyKeySuccess{
		Key:    keyName,
		Status: "ok",
		Action: "updated",
	}

	log.WithFields(logrus.Fields{
		"prefix": "api",
		"key":    keyName,
		"status": "ok",
	}).Info("Updated hashed key.")

	return statusObj, http.StatusOK
}

func (gw *Gateway) orgHandler(w http.ResponseWriter, r *http.Request) {
	orgID := mux.Vars(r)["keyName"]
	filter := r.URL.Query().Get("filter")
	var obj interface{}
	var code int

	switch r.Method {
	case "POST", "PUT":
		obj, code = gw.handleOrgAddOrUpdate(orgID, r)

	case "GET":
		if orgID != "" {
			// Return single org detail
			obj, code = gw.handleGetOrgDetail(orgID)
		} else {
			// Return list of keys
			obj, code = gw.handleGetAllOrgKeys(filter)
		}

	case "DELETE":
		// Remove a key
		obj, code = gw.handleDeleteOrgKey(orgID)
	}

	doJSONWrite(w, code, obj)
}

func (gw *Gateway) handleOrgAddOrUpdate(orgID string, r *http.Request) (interface{}, int) {
	newSession := new(user.SessionState)

	if err := json.NewDecoder(r.Body).Decode(newSession); err != nil {
		log.Error("Couldn't decode new session object: ", err)
		return apiError("Request malformed"), http.StatusBadRequest
	}
	// Update our session object (create it)

	spec := gw.getSpecForOrg(orgID)
	var sessionManager SessionHandler

	if spec == nil {
		log.Warning("Couldn't find org session store in active API list")
		if gw.GetConfig().SupressDefaultOrgStore {
			return apiError("No such organisation found in Active API list"), http.StatusNotFound
		}
		sessionManager = &gw.DefaultOrgStore
	} else {
		sessionManager = spec.OrgSessionManager
	}

	if r.URL.Query().Get("reset_quota") == "1" {
		sessionManager.ResetQuota(orgID, newSession, false)
		newSession.QuotaRenews = time.Now().Unix() + newSession.QuotaRenewalRate
		rawKey := QuotaKeyPrefix + storage.HashKey(orgID, gw.GetConfig().HashKeys)

		// manage quotas separately
		gw.DefaultQuotaStore.RemoveSession(orgID, rawKey, false)
	}

	err := sessionManager.UpdateSession(orgID, newSession, 0, false)
	if err != nil {
		return apiError("Error writing to key store " + err.Error()), http.StatusInternalServerError
	}

	// identify that spec has org session
	if spec != nil {
		spec.Lock()
		spec.OrgHasNoSession = false
		spec.Unlock()
	}

	log.WithFields(logrus.Fields{
		"prefix": "api",
		"org":    orgID,
		"status": "ok",
	}).Info("New organization key added or updated.")

	action := "modified"
	if r.Method == http.MethodPost {
		action = "added"
	}

	response := apiModifyKeySuccess{
		Key:    orgID,
		Status: "ok",
		Action: action,
	}

	return response, http.StatusOK
}

func (gw *Gateway) handleGetOrgDetail(orgID string) (interface{}, int) {
	spec := gw.getSpecForOrg(orgID)
	if spec == nil {
		return apiError("Org not found"), http.StatusNotFound
	}

	session, ok := spec.OrgSessionManager.SessionDetail(orgID, orgID, false)
	if !ok {
		log.WithFields(logrus.Fields{
			"prefix": "api",
			"org":    orgID,
			"status": "fail",
			"err":    "not found",
		}).Error("Failed retrieval of record for ORG ID.")
		return apiError("Org not found"), http.StatusNotFound
	}
	log.WithFields(logrus.Fields{
		"prefix": "api",
		"org":    orgID,
		"status": "ok",
	}).Info("Retrieved record for ORG ID.")
	return session.Clone(), http.StatusOK
}

func (gw *Gateway) handleGetAllOrgKeys(filter string) (interface{}, int) {
	spec := gw.getSpecForOrg("")
	if spec == nil {
		return apiError("ORG not found"), http.StatusNotFound
	}

	sessions := spec.OrgSessionManager.Sessions(filter)
	fixed_sessions := make([]string, 0)
	for _, s := range sessions {
		if !strings.HasPrefix(s, QuotaKeyPrefix) && !strings.HasPrefix(s, RateLimitKeyPrefix) {
			fixed_sessions = append(fixed_sessions, s)
		}
	}
	sessionsObj := apiAllKeys{fixed_sessions}
	return sessionsObj, http.StatusOK
}

func (gw *Gateway) handleDeleteOrgKey(orgID string) (interface{}, int) {
	spec := gw.getSpecForOrg(orgID)
	if spec == nil {
		log.WithFields(logrus.Fields{
			"prefix": "api",
			"key":    orgID,
			"status": "fail",
			"err":    "not found",
		}).Error("Failed to delete org key.")

		return apiError("Org not found"), http.StatusNotFound
	}

	if !spec.OrgSessionManager.RemoveSession(orgID, orgID, false) {
		return apiError("Failed to remove the key"), http.StatusBadRequest
	}

	log.WithFields(logrus.Fields{
		"prefix": "api",
		"key":    orgID,
		"status": "ok",
	}).Info("Org key deleted.")

	// identify that spec has no org session
	if spec != nil {
		spec.Lock()
		spec.OrgHasNoSession = true
		spec.Unlock()
	}

	statusObj := apiModifyKeySuccess{
		Key:    orgID,
		Status: "ok",
		Action: "deleted",
	}
	return statusObj, http.StatusOK
}

func (gw *Gateway) groupResetHandler(w http.ResponseWriter, r *http.Request) {
	log.WithFields(logrus.Fields{
		"prefix": "api",
		"status": "ok",
	}).Info("Group reload accepted.")

	// Signal to the group via redis
	gw.MainNotifier.Notify(Notification{Command: NoticeGroupReload, Gw: gw})

	log.WithFields(logrus.Fields{
		"prefix": "api",
	}).Info("Reloaded URL Structure - Success")

	doJSONWrite(w, http.StatusOK, apiOk(""))
}

// resetHandler will try to queue a reload. If fn is nil and block=true
// was in the URL parameters, it will block until the reload is done.
// Otherwise, it won't block and fn will be called once the reload is
// finished.
//
func (gw *Gateway) resetHandler(fn func()) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var wg sync.WaitGroup

		if fn == nil && r.URL.Query().Get("block") == "true" {
			wg.Add(1)
			gw.reloadURLStructure(wg.Done)
		} else {
			gw.reloadURLStructure(fn)
		}

		log.WithFields(logrus.Fields{
			"prefix": "api",
		}).Info("Reload URL Structure - Scheduled")
		wg.Wait()
		doJSONWrite(w, http.StatusOK, apiOk(""))
	}
}

func (gw *Gateway) createKeyHandler(w http.ResponseWriter, r *http.Request) {
	newSession := new(user.SessionState)
	if err := json.NewDecoder(r.Body).Decode(newSession); err != nil {
		log.WithFields(logrus.Fields{
			"prefix": "api",
			"status": "fail",
			"err":    err,
		}).Error("Key creation failed.")
		doJSONWrite(w, http.StatusInternalServerError, apiError("Unmarshalling failed"))
		return
	}

	newKey := gw.keyGen.GenerateAuthKey(newSession.OrgID)
	if newSession.HMACEnabled {
		newSession.HmacSecret = gw.keyGen.GenerateHMACSecret()
	}

	if newSession.Certificate != "" {
		newKey = gw.generateToken(newSession.OrgID, newSession.Certificate)
		_, ok := gw.GlobalSessionManager.SessionDetail(newSession.OrgID, newKey, false)
		if ok {
			doJSONWrite(w, http.StatusInternalServerError, apiError("Failed to create key - Key with given certificate already found:"+newKey))
			return
		}
	}

	newSession.LastUpdated = strconv.Itoa(int(time.Now().Unix()))
	newSession.DateCreated = time.Now()

	mw := BaseMiddleware{Gw: gw}
	// TODO: handle apply policies error
	mw.ApplyPolicies(newSession)

	if len(newSession.AccessRights) > 0 {
		// reset API-level limit to nil if any has a zero-value
		resetAPILimits(newSession.AccessRights)
		for apiID := range newSession.AccessRights {
			apiSpec := gw.getApiSpec(apiID)
			if apiSpec != nil {
				gw.checkAndApplyTrialPeriod(newKey, newSession, false)
				// If we have enabled HMAC checking for keys, we need to generate a secret for the client to use
				if !apiSpec.DontSetQuotasOnCreate {
					// Reset quota by default
					gw.GlobalSessionManager.ResetQuota(newKey, newSession, false)
					newSession.QuotaRenews = time.Now().Unix() + newSession.QuotaRenewalRate
				}
				// apply polices (if any) and save key
				if err := gw.applyPoliciesAndSave(newKey, newSession, apiSpec, false); err != nil {
					doJSONWrite(w, http.StatusInternalServerError, apiError("Failed to create key - "+err.Error()))
					return
				}
			} else {
				// Use fallback
				sessionManager := gw.GlobalSessionManager
				newSession.QuotaRenews = time.Now().Unix() + newSession.QuotaRenewalRate
				sessionManager.ResetQuota(newKey, newSession, false)
				err := sessionManager.UpdateSession(newKey, newSession, -1, false)
				if err != nil {
					doJSONWrite(w, http.StatusInternalServerError, apiError("Failed to create key - "+err.Error()))
					return
				}
			}
		}
	} else {
		if gw.GetConfig().AllowMasterKeys {
			// nothing defined, add key to ALL
			log.WithFields(logrus.Fields{
				"prefix":      "api",
				"status":      "warning",
				"org_id":      newSession.OrgID,
				"api_id":      "--",
				"user_id":     "system",
				"user_ip":     requestIPHops(r),
				"path":        "--",
				"server_name": "system",
			}).Warning("No API Access Rights set on key session, adding key to all APIs.")

			gw.apisMu.RLock()
			defer gw.apisMu.RUnlock()
			for _, spec := range gw.apisByID {
				gw.checkAndApplyTrialPeriod(newKey, newSession, false)
				if !spec.DontSetQuotasOnCreate {
					// Reset quote by default
					gw.GlobalSessionManager.ResetQuota(newKey, newSession, false)
					newSession.QuotaRenews = time.Now().Unix() + newSession.QuotaRenewalRate
				}
				// apply polices (if any) and save key
				if err := gw.applyPoliciesAndSave(newKey, newSession, spec, false); err != nil {
					doJSONWrite(w, http.StatusInternalServerError, apiError("Failed to create key - "+err.Error()))
					return
				}
			}
		} else {
			log.WithFields(logrus.Fields{
				"prefix":      "api",
				"status":      "error",
				"err":         "master keys disabled",
				"org_id":      newSession.OrgID,
				"api_id":      "--",
				"user_id":     "system",
				"user_ip":     requestIPHops(r),
				"path":        "--",
				"server_name": "system",
			}).Error("Master keys disallowed in configuration, key not added.")

			doJSONWrite(w, http.StatusBadRequest, apiError("Failed to create key, keys must have at least one Access Rights record set."))
			return
		}

	}

	obj := apiModifyKeySuccess{
		Action: "added",
		Key:    newKey,
		Status: "ok",
	}

	// add key hash to reply
	if gw.GetConfig().HashKeys {
		obj.KeyHash = storage.HashKey(newKey, gw.GetConfig().HashKeys)
	}

	gw.FireSystemEvent(EventTokenCreated, EventTokenMeta{
		EventMetaDefault: EventMetaDefault{Message: "Key generated."},
		Org:              newSession.OrgID,
		Key:              newKey,
	})

	log.WithFields(logrus.Fields{
		"prefix":      "api",
		"key":         gw.obfuscateKey(newKey),
		"status":      "ok",
		"api_id":      "--",
		"org_id":      newSession.OrgID,
		"user_id":     "system",
		"user_ip":     requestIPHops(r),
		"path":        "--",
		"server_name": "system",
	}).Info("Generated new key: (", gw.obfuscateKey(newKey), ")")

	doJSONWrite(w, http.StatusOK, obj)
}

func (gw *Gateway) previewKeyHandler(w http.ResponseWriter, r *http.Request) {
	newSession := new(user.SessionState)

	if err := json.NewDecoder(r.Body).Decode(newSession); err != nil {
		log.WithFields(logrus.Fields{
			"prefix": "api",
			"status": "fail",
			"err":    err,
		}).Error("Key creation failed.")
		doJSONWrite(w, http.StatusInternalServerError, apiError("Unmarshalling failed"))
		return
	}

	newSession.LastUpdated = strconv.Itoa(int(time.Now().Unix()))
	newSession.DateCreated = time.Now()

	mw := BaseMiddleware{Gw: gw}
	// TODO: handle apply policies error
	mw.ApplyPolicies(newSession)

	doJSONWrite(w, http.StatusOK, newSession)
}

// NewClientRequest is an outward facing JSON object translated from osin OAuthClients
//
// swagger:model NewClientRequest
type NewClientRequest struct {
	ClientID          string      `json:"client_id"`
	ClientRedirectURI string      `json:"redirect_uri"`
	APIID             string      `json:"api_id,omitempty"`
	PolicyID          string      `json:"policy_id,omitempty"`
	ClientSecret      string      `json:"secret"`
	MetaData          interface{} `json:"meta_data"`
	Description       string      `json:"description"`
}

func oauthClientStorageID(clientID string) string {
	return prefixClient + clientID
}

func (gw *Gateway) createOauthClient(w http.ResponseWriter, r *http.Request) {
	var newOauthClient NewClientRequest
	if err := json.NewDecoder(r.Body).Decode(&newOauthClient); err != nil {
		log.WithFields(logrus.Fields{
			"prefix": "api",
			"status": "fail",
			"err":    err,
		}).Error("Failed to create OAuth client")
		doJSONWrite(w, http.StatusInternalServerError, apiError("Unmarshalling failed"))
		return
	}

	// Allow the client ID to be set
	cleanSting := newOauthClient.ClientID

	if newOauthClient.ClientID == "" {
		u5 := uuid.NewV4()
		cleanSting = strings.Replace(u5.String(), "-", "", -1)
	}

	// Allow the secret to be set
	secret := newOauthClient.ClientSecret
	if newOauthClient.ClientSecret == "" {
		secret = createOauthClientSecret()
	}

	newClient := OAuthClient{
		ClientID:          cleanSting,
		ClientRedirectURI: newOauthClient.ClientRedirectURI,
		ClientSecret:      secret,
		PolicyID:          newOauthClient.PolicyID,
		MetaData:          newOauthClient.MetaData,
		Description:       newOauthClient.Description,
	}

	storageID := oauthClientStorageID(newClient.GetId())
	log.WithFields(logrus.Fields{
		"prefix": "api",
	}).Debug("Created storage ID: ", storageID)

	if newOauthClient.APIID != "" {
		// set client only for passed API ID
		apiSpec := gw.getApiSpec(newOauthClient.APIID)
		if apiSpec == nil {
			log.WithFields(logrus.Fields{
				"prefix": "api",
				"apiID":  newOauthClient.APIID,
				"status": "fail",
				"err":    "API doesn't exist",
			}).Error("Failed to create OAuth client")
			doJSONWrite(w, http.StatusBadRequest, apiError("API doesn't exist"))
			return
		}

		if !apiSpec.UseOauth2 {
			doJSONWrite(w, http.StatusBadRequest,
				apiError("API is not OAuth2"))
			return
		}

		err := apiSpec.OAuthManager.OsinServer.Storage.SetClient(storageID, apiSpec.OrgID, &newClient, true)
		if err != nil {
			log.WithFields(logrus.Fields{
				"prefix": "api",
				"apiID":  newOauthClient.APIID,
				"status": "fail",
				"err":    err,
			}).Error("Failed to create OAuth client")
			doJSONWrite(w, http.StatusInternalServerError, apiError("Failure in storing client data."))
			return
		}
	} else {
		// set client for all APIs from the given policy
		gw.policiesMu.RLock()
		policy, ok := gw.policiesByID[newClient.PolicyID]
		gw.policiesMu.RUnlock()
		if !ok {
			log.WithFields(logrus.Fields{
				"prefix":   "api",
				"policyID": newClient.PolicyID,
				"status":   "fail",
				"err":      "Policy doesn't exist",
			}).Error("Failed to create OAuth client")
			doJSONWrite(w, http.StatusBadRequest, apiError("Policy doesn't exist"))
			return
		}

		oauth2 := false
		// iterate over APIs and set client for each of them
		for apiID := range policy.AccessRights {
			apiSpec := gw.getApiSpec(apiID)
			if apiSpec == nil {
				log.WithFields(logrus.Fields{
					"prefix": "api",
					"apiID":  apiID,
					"status": "fail",
					"err":    "API doesn't exist",
				}).Error("Failed to create OAuth client")
				doJSONWrite(w, http.StatusBadRequest, apiError("API doesn't exist"))
				return
			}
			// set oauth client if it is oauth API
			if apiSpec.UseOauth2 || apiSpec.EnableJWT {
				oauth2 = true
				if apiSpec.OAuthManager == nil {

					prefix := generateOAuthPrefix(apiSpec.APIID)
					storageManager := gw.getGlobalMDCBStorageHandler(prefix, false)
					storageManager.Connect()

					apiSpec.OAuthManager = &OAuthManager{
						OsinServer: gw.TykOsinNewServer(
							&osin.ServerConfig{},
							&RedisOsinStorageInterface{
								storageManager,
								gw.GlobalSessionManager,
								&storage.RedisCluster{KeyPrefix: prefix, HashKeys: false, RedisController: gw.RedisController},
								apiSpec.OrgID,
								gw,
							}),
					}
				}
				err := apiSpec.OAuthManager.OsinServer.Storage.SetClient(storageID, apiSpec.APIDefinition.OrgID, &newClient, true)
				if err != nil {
					log.WithFields(logrus.Fields{
						"prefix": "api",
						"apiID":  apiID,
						"status": "fail",
						"err":    err,
					}).Error("Failed to create OAuth client")
					doJSONWrite(w, http.StatusInternalServerError, apiError("Failure in storing client data."))
					return
				}
			}
		}

		if !oauth2 {
			doJSONWrite(w, http.StatusBadRequest,
				apiError("API is not OAuth2"))
			return
		}
	}

	clientData := NewClientRequest{
		ClientID:          newClient.GetId(),
		ClientSecret:      newClient.GetSecret(),
		ClientRedirectURI: newClient.GetRedirectUri(),
		PolicyID:          newClient.GetPolicyID(),
		MetaData:          newClient.GetUserData(),
		Description:       newClient.GetDescription(),
	}

	log.WithFields(logrus.Fields{
		"prefix":            "api",
		"apiID":             newOauthClient.APIID,
		"clientID":          clientData.ClientID,
		"clientRedirectURI": clientData.ClientRedirectURI,
		"policyID":          clientData.PolicyID,
		"description":       clientData.Description,
		"status":            "ok",
	}).Info("Created OAuth client")

	doJSONWrite(w, http.StatusOK, clientData)
}

func (gw *Gateway) rotateOauthClient(keyName, apiID string) (interface{}, int) {
	// check API
	apiSpec := gw.getApiSpec(apiID)
	if apiSpec == nil {
		return apiError("API doesn't exist"), http.StatusNotFound
	}

	// get existing version of oauth-client
	storageID := oauthClientStorageID(keyName)
	client, err := apiSpec.OAuthManager.OsinServer.Storage.GetExtendedClientNoPrefix(storageID)
	if err != nil {
		return apiError("OAuth Client ID not found"), http.StatusNotFound
	}

	// update client
	updatedClient := OAuthClient{
		ClientID:          client.GetId(),
		ClientSecret:      createOauthClientSecret(),
		ClientRedirectURI: client.GetRedirectUri(),
		PolicyID:          client.GetPolicyID(),
		MetaData:          client.GetUserData(),
		Description:       client.GetDescription(),
	}

	err = apiSpec.OAuthManager.OsinServer.Storage.SetClient(storageID, apiSpec.OrgID, &updatedClient, true)
	if err != nil {
		log.WithFields(logrus.Fields{
			"prefix": "api",
			"apiID":  apiID,
			"status": "fail",
			"err":    err,
		}).Error("Failed to update OAuth client")
		return apiError("Failure in storing client data"), http.StatusInternalServerError
	}

	// invalidate tokens if we had a new policy
	invalidateTokens(client, updatedClient, apiSpec.OAuthManager)

	// convert to outbound format
	replyData := NewClientRequest{
		ClientID:          updatedClient.GetId(),
		ClientSecret:      updatedClient.ClientSecret,
		ClientRedirectURI: updatedClient.GetRedirectUri(),
		PolicyID:          updatedClient.GetPolicyID(),
		MetaData:          updatedClient.GetUserData(),
		Description:       updatedClient.GetDescription(),
	}

	return replyData, http.StatusOK
}

// Update Client
func (gw *Gateway) updateOauthClient(keyName, apiID string, r *http.Request) (interface{}, int) {
	// read payload
	var updateClientData NewClientRequest
	if err := json.NewDecoder(r.Body).Decode(&updateClientData); err != nil {
		log.WithFields(logrus.Fields{
			"prefix": "api",
			"status": "fail",
			"err":    err,
		}).Error("Failed to update OAuth client")
		return apiError("Unmarshalling failed"), http.StatusInternalServerError
	}

	// check API
	apiSpec := gw.getApiSpec(apiID)
	if apiSpec == nil {
		return apiError("API doesn't exist"), http.StatusNotFound
	}

	// check policy
	if updateClientData.PolicyID != "" {
		gw.policiesMu.RLock()
		policy, ok := gw.policiesByID[updateClientData.PolicyID]
		gw.policiesMu.RUnlock()
		if !ok {
			return apiError("Policy doesn't exist"), http.StatusNotFound
		}
		if _, ok := policy.AccessRights[apiID]; !ok {
			return apiError("Policy access rights doesn't contain API this OAuth client belongs to"),
				http.StatusBadRequest
		}
	}

	// get existing version of oauth-client
	storageID := oauthClientStorageID(keyName)
	client, err := apiSpec.OAuthManager.OsinServer.Storage.GetExtendedClientNoPrefix(storageID)
	if err != nil {
		return apiError("OAuth Client ID not found"), http.StatusNotFound
	}

	// update client
	updatedClient := OAuthClient{
		ClientID:          client.GetId(),
		ClientSecret:      client.GetSecret(),
		ClientRedirectURI: updateClientData.ClientRedirectURI, // update
		PolicyID:          updateClientData.PolicyID,          // update
		MetaData:          updateClientData.MetaData,          // update
		Description:       updateClientData.Description,       // update
	}

	err = apiSpec.OAuthManager.OsinServer.Storage.SetClient(storageID, apiSpec.OrgID, &updatedClient, true)
	if err != nil {
		log.WithFields(logrus.Fields{
			"prefix": "api",
			"apiID":  apiID,
			"status": "fail",
			"err":    err,
		}).Error("Failed to update OAuth client")
		return apiError("Failure in storing client data"), http.StatusInternalServerError
	}

	// invalidate tokens if we had a new policy
	invalidateTokens(client, updatedClient, apiSpec.OAuthManager)

	// convert to outbound format
	replyData := NewClientRequest{
		ClientID:          updatedClient.GetId(),
		ClientSecret:      updatedClient.GetSecret(),
		ClientRedirectURI: updatedClient.GetRedirectUri(),
		PolicyID:          updatedClient.GetPolicyID(),
		MetaData:          updatedClient.GetUserData(),
		Description:       updatedClient.GetDescription(),
	}

	return replyData, http.StatusOK
}

func (gw *Gateway) invalidateOauthRefresh(w http.ResponseWriter, r *http.Request) {
	apiID := r.URL.Query().Get("api_id")
	if apiID == "" {
		doJSONWrite(w, http.StatusBadRequest, apiError("Missing parameter api_id"))
		return
	}
	apiSpec := gw.getApiSpec(apiID)

	log.WithFields(logrus.Fields{
		"prefix": "api",
		"apiID":  apiID,
	}).Debug("Looking for refresh token in API Register")

	if apiSpec == nil {
		log.WithFields(logrus.Fields{
			"prefix": "api",
			"apiID":  apiID,
			"status": "fail",
			"err":    "API not found",
		}).Error("Failed to invalidate refresh token")

		doJSONWrite(w, http.StatusNotFound, apiError("API for this refresh token not found"))
		return
	}

	if apiSpec.OAuthManager == nil {
		log.WithFields(logrus.Fields{
			"prefix": "api",
			"apiID":  apiID,
			"status": "fail",
			"err":    "API is not OAuth",
		}).Error("Failed to invalidate refresh token")

		doJSONWrite(w, http.StatusBadRequest, apiError("OAuth is not enabled on this API"))
		return
	}

	keyName := mux.Vars(r)["keyName"]
	err := apiSpec.OAuthManager.OsinServer.Storage.RemoveRefresh(keyName)
	if err != nil {
		log.WithFields(logrus.Fields{
			"prefix": "api",
			"apiID":  apiID,
			"status": "fail",
			"err":    err,
		}).Error("Failed to invalidate refresh token")

		doJSONWrite(w, http.StatusInternalServerError, apiError("Failed to invalidate refresh token"))
		return
	}

	success := apiModifyKeySuccess{
		Key:    keyName,
		Status: "ok",
		Action: "deleted",
	}

	log.WithFields(logrus.Fields{
		"prefix": "api",
		"apiID":  apiID,
		"token":  keyName,
		"status": "ok",
	}).Info("Invalidated refresh token")

	doJSONWrite(w, http.StatusOK, success)
}

func (gw *Gateway) rotateOauthClientHandler(w http.ResponseWriter, r *http.Request) {

	apiID := mux.Vars(r)["apiID"]
	keyName := mux.Vars(r)["keyName"]

	obj, code := gw.rotateOauthClient(keyName, apiID)

	doJSONWrite(w, code, obj)
}

func (gw *Gateway) getApisForOauthApp(w http.ResponseWriter, r *http.Request) {
	apis := []string{}
	appID := mux.Vars(r)["appID"]
	orgID := r.FormValue("orgID")

	//get all organization apis
	apisIds := gw.getApisIdsForOrg(orgID)

	for index := range apisIds {
		if api := gw.getApiSpec(apisIds[index]); api != nil {
			if api.UseOauth2 {
				clients, _, code := gw.getApiClients(apisIds[index])
				if code == http.StatusOK {
					for _, client := range clients {
						if client.GetId() == appID {
							apis = append(apis, apisIds[index])
						}
					}
				}
			}
		}
	}

	doJSONWrite(w, http.StatusOK, apis)
}

func (gw *Gateway) oAuthClientHandler(w http.ResponseWriter, r *http.Request) {
	apiID := mux.Vars(r)["apiID"]
	keyName := mux.Vars(r)["keyName"]

	var obj interface{}
	var code int
	switch r.Method {
	case http.MethodGet:
		if keyName != "" {
			// Return single client detail
			obj, code = gw.getOauthClientDetails(keyName, apiID)
		} else {
			// Return list of keys
			obj, code = gw.getOauthClients(apiID)
		}
	case http.MethodPut:
		// Update client
		obj, code = gw.updateOauthClient(keyName, apiID, r)
	case http.MethodDelete:
		// Remove a key
		obj, code = gw.handleDeleteOAuthClient(keyName, apiID)
	}

	doJSONWrite(w, code, obj)
}

func (gw *Gateway) oAuthClientTokensHandler(w http.ResponseWriter, r *http.Request) {
	apiID := mux.Vars(r)["apiID"]
	keyName := mux.Vars(r)["keyName"]

	apiSpec := gw.getApiSpec(apiID)
	if apiSpec == nil {
		log.WithFields(logrus.Fields{
			"prefix": "api",
			"apiID":  apiID,
			"status": "fail",
			"client": keyName,
			"err":    "not found",
		}).Error("Failed to retrieve OAuth tokens")
		doJSONWrite(w, http.StatusNotFound, apiError("OAuth Client ID not found"))
		return
	}

	if p := r.URL.Query().Get("page"); p != "" {
		page := 1

		queryPage, err := strconv.Atoi(p)
		if err == nil {
			page = queryPage
		}

		if page <= 0 {
			page = 1
		}

		tokens, totalPages, err := apiSpec.OAuthManager.OsinServer.Storage.GetPaginatedClientTokens(keyName, page)
		if err != nil {
			doJSONWrite(w, http.StatusInternalServerError, apiError("Get client tokens failed"))
			return
		}

		doJSONWrite(w, http.StatusOK, paginatedOAuthClientTokens{
			Pagination: paginationStatus{
				PageSize:  100,
				PageNum:   page,
				PageTotal: totalPages,
			},
			Tokens: tokens,
		})

		return
	}

	tokens, err := apiSpec.OAuthManager.OsinServer.Storage.GetClientTokens(keyName)
	if err != nil {
		doJSONWrite(w, http.StatusInternalServerError, apiError("Get client tokens failed"))
		return
	}

	doJSONWrite(w, http.StatusOK, tokens)
}

// Get client details
func (gw *Gateway) getOauthClientDetails(keyName, apiID string) (interface{}, int) {
	storageID := oauthClientStorageID(keyName)
	apiSpec := gw.getApiSpec(apiID)
	if apiSpec == nil {
		log.WithFields(logrus.Fields{
			"prefix": "api",
			"apiID":  apiID,
			"status": "fail",
			"client": keyName,
			"err":    "not found",
		}).Error("Failed to retrieve OAuth client details")
		return apiError("OAuth Client ID not found"), http.StatusNotFound
	}

	if apiSpec.OAuthManager == nil {
		prefix := generateOAuthPrefix(apiSpec.APIID)
		storageManager := gw.getGlobalMDCBStorageHandler(prefix, false)
		storageManager.Connect()
		apiSpec.OAuthManager = &OAuthManager{
			OsinServer: gw.TykOsinNewServer(&osin.ServerConfig{},
				&RedisOsinStorageInterface{
					storageManager,
					gw.GlobalSessionManager,
					&storage.RedisCluster{KeyPrefix: prefix, HashKeys: false, RedisController: gw.RedisController},
					apiSpec.OrgID,
					gw,
				}),
		}
	}

	clientData, err := apiSpec.OAuthManager.OsinServer.Storage.GetExtendedClientNoPrefix(storageID)
	if err != nil {
		return apiError("OAuth Client ID not found"), http.StatusNotFound
	}
	reportableClientData := NewClientRequest{
		ClientID:          clientData.GetId(),
		ClientSecret:      clientData.GetSecret(),
		ClientRedirectURI: clientData.GetRedirectUri(),
		PolicyID:          clientData.GetPolicyID(),
		MetaData:          clientData.GetUserData(),
		Description:       clientData.GetDescription(),
	}

	log.WithFields(logrus.Fields{
		"prefix": "api",
		"apiID":  apiID,
		"status": "ok",
		"client": keyName,
	}).Info("Retrieved OAuth client ID")

	return reportableClientData, http.StatusOK
}

// Delete Client
func (gw *Gateway) handleDeleteOAuthClient(keyName, apiID string) (interface{}, int) {
	storageID := oauthClientStorageID(keyName)

	apiSpec := gw.getApiSpec(apiID)
	if apiSpec == nil {
		log.WithFields(logrus.Fields{
			"prefix": "api",
			"apiID":  apiID,
			"status": "fail",
			"client": keyName,
			"err":    "not found",
		}).Error("Failed to delete OAuth client")

		return apiError("OAuth Client ID not found"), http.StatusNotFound
	}

	if apiSpec.OAuthManager != nil {
		err := apiSpec.OAuthManager.OsinServer.Storage.DeleteClient(storageID, apiSpec.OrgID, true)
		if err != nil {
			return apiError("Delete failed"), http.StatusInternalServerError
		}

		statusObj := apiModifyKeySuccess{
			Key:    keyName,
			Status: "ok",
			Action: "deleted",
		}

		log.WithFields(logrus.Fields{
			"prefix": "api",
			"apiID":  apiID,
			"status": "ok",
			"client": keyName,
		}).Info("Deleted OAuth client")

		return statusObj, http.StatusOK
	}

	return apiError("OAuth Client ID not found"), http.StatusNotFound
}

const oAuthNotPropagatedErr = "OAuth client list isn't available or hasn't been propagated yet."
const oAuthClientNotFound = "OAuth client not found"
const oauthClientIdEmpty = "client_id is required"
const oauthClientSecretEmpty = "client_secret is required"
const oauthClientSecretWrong = "client secret is wrong"
const oauthTokenEmpty = "token is required"

func (gw *Gateway) getApiClients(apiID string) ([]ExtendedOsinClientInterface, apiStatusMessage, int) {
	var err error
	filterID := prefixClient
	apiSpec := gw.getApiSpec(apiID)

	if apiSpec == nil {
		log.WithFields(logrus.Fields{
			"prefix": "api",
			"apiID":  apiID,
			"status": "fail",
			"err":    "API not found",
		}).Error("Failed to retrieve OAuth client list.")
		return nil, apiError(oAuthClientNotFound), http.StatusNotFound
	}

	clientData := []ExtendedOsinClientInterface{}
	if apiSpec.UseOauth2 {
		clientData, err = apiSpec.OAuthManager.OsinServer.Storage.GetClients(filterID, apiSpec.OrgID, true)
		if err != nil {
			log.WithFields(logrus.Fields{
				"prefix": "api",
				"apiID":  apiID,
				"status": "fail",
				"err":    err,
			}).Error("Failed to report OAuth client list")

			return nil, apiError(oAuthClientNotFound), http.StatusNotFound
		}
	}

	return clientData, apiStatusMessage{}, http.StatusOK
}

// List Clients
func (gw *Gateway) getOauthClients(apiID string) (interface{}, int) {

	clientData, _, apiStatusCode := gw.getApiClients(apiID)

	if apiStatusCode != 200 {
		return clientData, apiStatusCode
	}

	clients := []NewClientRequest{}
	for _, osinClient := range clientData {
		reportableClientData := NewClientRequest{
			ClientID:          osinClient.GetId(),
			ClientSecret:      osinClient.GetSecret(),
			ClientRedirectURI: osinClient.GetRedirectUri(),
			PolicyID:          osinClient.GetPolicyID(),
			MetaData:          osinClient.GetUserData(),
			Description:       osinClient.GetDescription(),
		}

		clients = append(clients, reportableClientData)
	}
	log.WithFields(logrus.Fields{
		"prefix": "api",
		"apiID":  apiID,
		"status": "ok",
	}).Info("Retrieved OAuth client list")

	return clients, http.StatusOK
}

func (gw *Gateway) getApisForOauthClientId(oauthClientId string, orgId string) []string {
	apis := []string{}
	orgApis := gw.getApisIdsForOrg(orgId)

	for index := range orgApis {
		clientsData, _, status := gw.getApiClients(orgApis[index])
		if status == http.StatusOK {
			for _, client := range clientsData {
				if client.GetId() == oauthClientId {
					apis = append(apis, orgApis[index])
				}
			}
		}
	}

	return apis
}

func (gw *Gateway) healthCheckhandler(w http.ResponseWriter, r *http.Request) {
	if !gw.GetConfig().HealthCheck.EnableHealthChecks {
		doJSONWrite(w, http.StatusBadRequest, apiError("Health checks are not enabled for this node"))
		return
	}
	apiID := r.URL.Query().Get("api_id")
	if apiID == "" {
		doJSONWrite(w, http.StatusBadRequest, apiError("missing api_id parameter"))
		return
	}
	apiSpec := gw.getApiSpec(apiID)
	if apiSpec == nil {
		doJSONWrite(w, http.StatusNotFound, apiError("API ID not found"))
		return
	}
	health, _ := apiSpec.Health.ApiHealthValues()
	doJSONWrite(w, http.StatusOK, health)
}

func userRatesCheck(w http.ResponseWriter, r *http.Request) {
	session := ctxGetSession(r)
	if session == nil {
		doJSONWrite(w, http.StatusBadRequest, apiError("Health checks are not enabled for this node"))
		return
	}

	returnSession := PublicSession{}
	returnSession.Quota.QuotaRenews = session.QuotaRenews
	returnSession.Quota.QuotaRemaining = session.QuotaRemaining
	returnSession.Quota.QuotaMax = session.QuotaMax
	returnSession.RateLimit.Rate = session.Rate
	returnSession.RateLimit.Per = session.Per

	doJSONWrite(w, http.StatusOK, returnSession)
}

func (gw *Gateway) invalidateCacheHandler(w http.ResponseWriter, r *http.Request) {
	apiID := mux.Vars(r)["apiID"]

	keyPrefix := "cache-" + apiID
	matchPattern := keyPrefix + "*"
	store := storage.RedisCluster{KeyPrefix: keyPrefix, IsCache: true, RedisController: gw.RedisController}

	if ok := store.DeleteScanMatch(matchPattern); !ok {
		err := errors.New("scan/delete failed")
		var orgid string
		if spec := gw.getApiSpec(apiID); spec != nil {
			orgid = spec.OrgID
		}
		log.WithFields(logrus.Fields{
			"prefix":      "api",
			"api_id":      apiID,
			"status":      "fail",
			"err":         err,
			"org_id":      orgid,
			"user_id":     "system",
			"user_ip":     requestIPHops(r),
			"path":        "--",
			"server_name": "system",
		}).Error("Failed to delete cache: ", err)

		doJSONWrite(w, http.StatusInternalServerError, apiError("Cache invalidation failed"))
		return
	}

	doJSONWrite(w, http.StatusOK, apiOk("cache invalidated"))
}

func (gw *Gateway) RevokeTokenHandler(w http.ResponseWriter, r *http.Request) {
	err := r.ParseForm()

	if err != nil {
		doJSONWrite(w, http.StatusBadRequest, apiError("cannot parse form. Form malformed"))
		return
	}

	tokenTypeHint := r.PostFormValue("token_type_hint")
	token := r.PostFormValue("token")
	clientID := r.PostFormValue("client_id")
	orgID := r.PostFormValue("org_id")

	if token == "" {
		doJSONWrite(w, http.StatusBadRequest, apiError(oauthTokenEmpty))
		return
	}

	if clientID == "" {
		doJSONWrite(w, http.StatusBadRequest, apiError(oauthClientIdEmpty))
		return
	}

	apis := gw.getApisForOauthClientId(clientID, orgID)
	if len(apis) == 0 {
		doJSONWrite(w, http.StatusBadRequest, apiError("oauth client doesn't exist"))
		return
	}

	for _, apiID := range apis {
		storage, _, err := gw.GetStorageForApi(apiID)
		if err == nil {
			RevokeToken(storage, token, tokenTypeHint)
		}
	}
	doJSONWrite(w, http.StatusOK, apiOk("token revoked successfully"))
}

func (gw *Gateway) GetStorageForApi(apiID string) (ExtendedOsinStorageInterface, int, error) {
	apiSpec := gw.getApiSpec(apiID)
	if apiSpec == nil {
		log.WithFields(logrus.Fields{
			"prefix": "api",
			"apiID":  apiID,
			"status": "fail",
			"err":    "API not found",
		}).Error("Failed to retrieve OAuth client list.")

		return nil, http.StatusNotFound, errors.New(oAuthClientNotFound)
	}

	if apiSpec.OAuthManager == nil {
		log.WithFields(logrus.Fields{
			"prefix": "api",
			"apiID":  apiID,
			"status": "fail",
			"err":    "API not found",
		}).Error("Failed to revoke client tokens.")

		return nil, http.StatusNotFound, errors.New(oAuthNotPropagatedErr)
	}

	return apiSpec.OAuthManager.OsinServer.Storage, http.StatusOK, nil
}

func (gw *Gateway) RevokeAllTokensHandler(w http.ResponseWriter, r *http.Request) {
	err := r.ParseForm()

	if err != nil {
		doJSONWrite(w, http.StatusBadRequest, apiError("cannot parse form. Form malformed"))
		return
	}

	clientId := r.PostFormValue("client_id")
	clientSecret := r.PostFormValue("client_secret")
	orgId := r.PostFormValue("org_id")

	if clientId == "" {
		doJSONWrite(w, http.StatusUnauthorized, apiError(oauthClientIdEmpty))
		return
	}

	if clientSecret == "" {
		doJSONWrite(w, http.StatusUnauthorized, apiError(oauthClientSecretEmpty))
		return
	}

	apis := gw.getApisForOauthClientId(clientId, orgId)
	if len(apis) == 0 {
		//if api is 0 is because the client wasn't found
		doJSONWrite(w, http.StatusNotFound, apiError("oauth client doesn't exist"))
		return
	}

	tokens := []string{}
	for _, apiId := range apis {
		storage, _, err := gw.GetStorageForApi(apiId)
		if err == nil {
			_, tokensRevoked, _ := RevokeAllTokens(storage, clientId, clientSecret)
			tokens = append(tokens, tokensRevoked...)
		}
	}

	n := Notification{
		Command: KeySpaceUpdateNotification,
		Payload: strings.Join(tokens, ","),
		Gw:      gw,
	}
	gw.MainNotifier.Notify(n)

	doJSONWrite(w, http.StatusOK, apiOk("tokens revoked successfully"))
}

// TODO: Don't modify http.Request values in-place. We must right now
// because our middleware design doesn't pass around http.Request
// pointers, so we have no way to modify the pointer in a middleware.
//
// If we ever redesign middlewares - or if we find another workaround -
// revisit this.
func setContext(r *http.Request, ctx context.Context) {
	r2 := r.WithContext(ctx)
	*r = *r2
}
func setCtxValue(r *http.Request, key, val interface{}) {
	setContext(r, context.WithValue(r.Context(), key, val))
}

func ctxGetData(r *http.Request) map[string]interface{} {
	if v := r.Context().Value(ctx.ContextData); v != nil {
		return v.(map[string]interface{})
	}
	return nil
}

func ctxSetData(r *http.Request, m map[string]interface{}) {
	if m == nil {
		panic("setting a nil context ContextData")
	}
	setCtxValue(r, ctx.ContextData, m)
}

func ctxGetSession(r *http.Request) *user.SessionState {
	return ctx.GetSession(r)
}

func ctxSetSession(r *http.Request, s *user.SessionState, scheduleUpdate bool, hashKey bool) {
	ctx.SetSession(r, s, scheduleUpdate, hashKey)
}

func ctxScheduleSessionUpdate(r *http.Request) {
	setCtxValue(r, ctx.UpdateSession, true)
}

func ctxDisableSessionUpdate(r *http.Request) {
	setCtxValue(r, ctx.UpdateSession, false)
}

func ctxSessionUpdateScheduled(r *http.Request) bool {
	if v := r.Context().Value(ctx.UpdateSession); v != nil {
		return v.(bool)
	}
	return false
}

func ctxGetAuthToken(r *http.Request) string {
	return ctx.GetAuthToken(r)
}

func ctxGetTrackedPath(r *http.Request) string {
	if v := r.Context().Value(ctx.TrackThisEndpoint); v != nil {
		return v.(string)
	}
	return ""
}

func ctxSetTrackedPath(r *http.Request, p string) {
	if p == "" {
		panic("setting a nil context TrackThisEndpoint")
	}
	setCtxValue(r, ctx.TrackThisEndpoint, p)
}

func ctxGetDoNotTrack(r *http.Request) bool {
	return r.Context().Value(ctx.DoNotTrackThisEndpoint) == true
}

func ctxSetDoNotTrack(r *http.Request, b bool) {
	setCtxValue(r, ctx.DoNotTrackThisEndpoint, b)
}

func ctxGetVersionInfo(r *http.Request) *apidef.VersionInfo {
	if v := r.Context().Value(ctx.VersionData); v != nil {
		return v.(*apidef.VersionInfo)
	}
	return nil
}

func ctxSetVersionInfo(r *http.Request, v *apidef.VersionInfo) {
	setCtxValue(r, ctx.VersionData, v)
}

func ctxGetVersionName(r *http.Request) *string {
	if v := r.Context().Value(ctx.VersionName); v != nil {
		return v.(*string)
	}

	return nil
}

func ctxSetVersionName(r *http.Request, vName *string) {
	setCtxValue(r, ctx.VersionName, vName)
}

func ctxSetOrigRequestURL(r *http.Request, url *url.URL) {
	setCtxValue(r, ctx.OrigRequestURL, url)
}

func ctxGetOrigRequestURL(r *http.Request) *url.URL {
	if v := r.Context().Value(ctx.OrigRequestURL); v != nil {
		if urlVal, ok := v.(*url.URL); ok {
			return urlVal
		}
	}

	return nil
}

func ctxSetURLRewriteTarget(r *http.Request, url *url.URL) {
	setCtxValue(r, ctx.UrlRewriteTarget, url)
}

func ctxGetURLRewriteTarget(r *http.Request) *url.URL {
	if v := r.Context().Value(ctx.UrlRewriteTarget); v != nil {
		if urlVal, ok := v.(*url.URL); ok {
			return urlVal
		}
	}

	return nil
}

func ctxSetUrlRewritePath(r *http.Request, path string) {
	setCtxValue(r, ctx.UrlRewritePath, path)
}

func ctxGetUrlRewritePath(r *http.Request) string {
	if v := r.Context().Value(ctx.UrlRewritePath); v != nil {
		if strVal, ok := v.(string); ok {
			return strVal
		}
	}
	return ""
}

func ctxSetCheckLoopLimits(r *http.Request, b bool) {
	setCtxValue(r, ctx.CheckLoopLimits, b)
}

// Should we check Rate limits and Quotas?
func ctxCheckLimits(r *http.Request) bool {
	// If looping disabled, allow all
	if !ctxLoopingEnabled(r) {
		return true
	}

	if v := r.Context().Value(ctx.CheckLoopLimits); v != nil {
		return v.(bool)
	}

	return false
}

func ctxSetRequestMethod(r *http.Request, path string) {
	setCtxValue(r, ctx.RequestMethod, path)
}

func ctxGetRequestMethod(r *http.Request) string {
	if v := r.Context().Value(ctx.RequestMethod); v != nil {
		if strVal, ok := v.(string); ok {
			return strVal
		}
	}
	return r.Method
}

func ctxSetTransformRequestMethod(r *http.Request, path string) {
	setCtxValue(r, ctx.TransformedRequestMethod, path)
}

func ctxGetTransformRequestMethod(r *http.Request) string {
	if v := r.Context().Value(ctx.TransformedRequestMethod); v != nil {
		if strVal, ok := v.(string); ok {
			return strVal
		}
	}
	return r.Method
}

func ctxSetGraphQLRequest(r *http.Request, gqlRequest *gql.Request) {
	setCtxValue(r, ctx.GraphQLRequest, gqlRequest)
}

func ctxGetGraphQLRequest(r *http.Request) (gqlRequest *gql.Request) {
	if v := r.Context().Value(ctx.GraphQLRequest); v != nil {
		if gqlRequest, ok := v.(*gql.Request); ok {
			return gqlRequest
		}
	}
	return nil
}

func ctxSetGraphQLIsWebSocketUpgrade(r *http.Request, isWebSocketUpgrade bool) {
	setCtxValue(r, ctx.GraphQLIsWebSocketUpgrade, isWebSocketUpgrade)
}

func ctxGetGraphQLIsWebSocketUpgrade(r *http.Request) (isWebSocketUpgrade bool) {
	if v := r.Context().Value(ctx.GraphQLIsWebSocketUpgrade); v != nil {
		if isWebSocketUpgrade, ok := v.(bool); ok {
			return isWebSocketUpgrade
		}
	}

	return false
}

func ctxGetDefaultVersion(r *http.Request) bool {
	return r.Context().Value(ctx.VersionDefault) != nil
}

func ctxSetDefaultVersion(r *http.Request) {
	setCtxValue(r, ctx.VersionDefault, true)
}

func ctxLoopingEnabled(r *http.Request) bool {
	return ctxLoopLevel(r) > 0
}

func ctxLoopLevel(r *http.Request) int {
	if v := r.Context().Value(ctx.LoopLevel); v != nil {
		if intVal, ok := v.(int); ok {
			return intVal
		}
	}

	return 0
}

func ctxSetLoopLevel(r *http.Request, value int) {
	setCtxValue(r, ctx.LoopLevel, value)
}

func ctxIncLoopLevel(r *http.Request, loopLimit int) {
	ctxSetLoopLimit(r, loopLimit)
	ctxSetLoopLevel(r, ctxLoopLevel(r)+1)
}

func ctxLoopLevelLimit(r *http.Request) int {
	if v := r.Context().Value(ctx.LoopLevelLimit); v != nil {
		if intVal, ok := v.(int); ok {
			return intVal
		}
	}

	return 0
}

func ctxSetLoopLimit(r *http.Request, limit int) {
	// Can be set only one time per request
	if ctxLoopLevelLimit(r) == 0 && limit > 0 {
		setCtxValue(r, ctx.LoopLevelLimit, limit)
	}
}

func ctxThrottleLevelLimit(r *http.Request) int {
	if v := r.Context().Value(ctx.ThrottleLevelLimit); v != nil {
		if intVal, ok := v.(int); ok {
			return intVal
		}
	}

	return 0
}

func ctxThrottleLevel(r *http.Request) int {
	if v := r.Context().Value(ctx.ThrottleLevel); v != nil {
		if intVal, ok := v.(int); ok {
			return intVal
		}
	}

	return 0
}

func ctxSetThrottleLimit(r *http.Request, limit int) {
	// Can be set only one time per request
	if ctxThrottleLevelLimit(r) == 0 && limit > 0 {
		setCtxValue(r, ctx.ThrottleLevelLimit, limit)
	}
}

func ctxSetThrottleLevel(r *http.Request, value int) {
	setCtxValue(r, ctx.ThrottleLevel, value)
}

func ctxIncThrottleLevel(r *http.Request, throttleLimit int) {
	ctxSetThrottleLimit(r, throttleLimit)
	ctxSetThrottleLevel(r, ctxThrottleLevel(r)+1)
}

func ctxTraceEnabled(r *http.Request) bool {
	return r.Context().Value(ctx.Trace) != nil
}

func ctxSetTrace(r *http.Request) {
	setCtxValue(r, ctx.Trace, true)
}

func ctxSetRequestStatus(r *http.Request, stat RequestStatus) {
	setCtxValue(r, ctx.RequestStatus, stat)
}

func ctxGetRequestStatus(r *http.Request) (stat RequestStatus) {
	if v := r.Context().Value(ctx.RequestStatus); v != nil {
		stat = v.(RequestStatus)
	}
	return
}

var createOauthClientSecret = func() string {
	secret := uuid.NewV4()
	return base64.StdEncoding.EncodeToString([]byte(secret.String()))
}

// invalidate tokens if we had a new policy
func invalidateTokens(prevClient ExtendedOsinClientInterface, updatedClient OAuthClient, oauthManager *OAuthManager) {

	if prevPolicy := prevClient.GetPolicyID(); prevPolicy != "" && prevPolicy != updatedClient.PolicyID {
		tokenList, err := oauthManager.OsinServer.Storage.GetClientTokens(updatedClient.ClientID)
		if err != nil {
			log.WithError(err).Warning("Could not get list of tokens for updated OAuth client")
		}

		for _, token := range tokenList {
			if err := oauthManager.OsinServer.Storage.RemoveAccess(token.Token); err != nil {
				log.WithError(err).Warning("Could not remove token for updated OAuth client policy")
			}
		}
	}
}
