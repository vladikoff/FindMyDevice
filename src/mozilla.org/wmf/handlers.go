package wmf

/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

import (
	"code.google.com/p/go.net/websocket"
	"github.com/gorilla/sessions"
	"mozilla.org/util"
	"mozilla.org/wmf/storage"

	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

// base handler for REST and Socket calls.
type Handler struct {
	config  *util.MzConfig
	logger  *util.HekaLogger
	metrics *util.Metrics
	devId   string
	logCat  string
	accepts []string
	hawk    *Hawk
	session *sessions.CookieStore
}

// Map of clientIDs to socket handlers
type ClientMap map[string]*WWS

const (
	SESSION_NAME     = "user"
	OAUTH_ENDPOINT   = "https://oauth.accounts.firefox.com"
	CONTENT_ENDPOINT = "https://accounts.firefox.com"
)

var (
	muClient sync.Mutex
	Clients  = make(ClientMap)
)

// Generic reply structure (useful for JSON responses)
type replyType map[string]interface{}

// Each session contains a UserID and a DeviceID
type sessionInfoStruct struct {
	UserId      string
	DeviceId    string
	User        string
	AccessToken string
}

var ErrInvalidReply = errors.New("Invalid Command Response")
var ErrAuthorization = errors.New("Needs Authorization")
var ErrNoUser = errors.New("No User")
var ErrOauth = errors.New("OAuth Error")

//Handler private functions

// verify a Persona assertion using the config values
// part of Handler for config & logging reasons
func (self *Handler) verifyPersonaAssertion(assertion string) (userid, email string, err error) {
	var ok bool
	if assLen := len(assertion); assLen != len(strings.Map(assertionFilter, assertion)) {
		self.logger.Error(self.logCat, "Assertion contains invalid characters.",
			util.Fields{"assertion": assertion})
		return "", "", ErrAuthorization
	}

	if self.config.GetFlag("auth.disabled") {
		self.logger.Warn(self.logCat, "!!! Skipping validation...", nil)
		if len(assertion) == 0 {
			return "user1", "user@example.com", nil
		}
		// Time to UberFake! THIS IS VERY DANGEROUS!
		self.logger.Warn(self.logCat,
			"!!! Using Assertion Without Validation",
			nil)
		bits := strings.Split(assertion, ".")
		if len(bits) < 2 {
			self.logger.Error(self.logCat, "Invalid assertion",
				util.Fields{"assertion": assertion})
			return "", "", errors.New("Invalid assertion")
		}
		// get the interesting bit
		intBit := bits[1]
		// pad to byte boundry
		intBit = intBit + "===="[:len(intBit)%4]
		decoded, err := base64.StdEncoding.DecodeString(intBit)
		if err != nil {
			self.logger.Error(self.logCat, "Could not decode assertion",
				util.Fields{"error": err.Error()})
			return "", "", err
		}
		asrt := make(replyType)
		err = json.Unmarshal(decoded, &asrt)
		if err != nil {
			self.logger.Error(self.logCat, "Could not unmarshal",
				util.Fields{"error": err.Error()})
			return "", "", err
		}
		// Normally, the UserID would be provided from FxA.
		// since FxA is currently unavailable for desktop, we're going
		// to need a value here, thus the insecure Id generation.
		// Obviously:
		// ******** DO NOT ENABLE auth.disabled FLAG IN PRODUCTION!! ******
		email = asrt["principal"].(map[string]interface{})["email"].(string)
		hasher := sha256.New()
		hasher.Write([]byte(email))
		userid = hex.EncodeToString(hasher.Sum(nil))
		userid = self.genHash(email)
		self.logger.Debug(self.logCat, "Extracted credentials",
			util.Fields{"userId": userid, "email": email})
		return userid, email, nil
	}

	// Better verify for realz
	validatorURL := self.config.Get("persona.validater_url",
		"https://verifier.login.persona.org/verify")
	audience := self.config.Get("persona.audience",
		"http://localhost:8080")
	body, err := json.Marshal(
		util.Fields{"assertion": assertion,
			"audience": audience})
	if err != nil {
		self.logger.Error(self.logCat,
			"Could not marshal assertion",
			util.Fields{"error": err.Error()})
		return "", "", ErrAuthorization
	}
	req, err := http.NewRequest("POST", validatorURL, bytes.NewReader(body))
	if err != nil {
		self.logger.Error(self.logCat, "Could not POST assertion",
			util.Fields{"error": err.Error()})
		return "", "", err
	}
	req.Header.Add("Content-Type", "application/json")
	cli := http.Client{}
	res, err := cli.Do(req)
	if err != nil {
		self.logger.Error(self.logCat, "Persona verification failed",
			util.Fields{"error": err.Error()})
		return "", "", ErrAuthorization
	}

	// Handle the verifier response
	buffer, err := parseBody(res.Body)
	if isOk, ok := buffer["status"]; !ok || isOk != "okay" {
		var errStr string
		if err != nil {
			errStr = err.Error()
		} else if _, ok = buffer["reason"]; ok {
			errStr = buffer["reason"].(string)
		}
		self.logger.Error(self.logCat, "Persona Auth Failed",
			util.Fields{"error": errStr,
				"buffer": fmt.Sprintf("%+v", buffer)})
		return "", "", ErrAuthorization
	}

	// extract the email
	if email, ok = buffer["email"].(string); !ok {
		self.logger.Error(self.logCat, "No email found in assertion",
			util.Fields{"assertion": fmt.Sprintf("%+v", buffer)})
		return "", "", ErrAuthorization
	}
	// and the userid, generating one if need be.
	if _, ok = buffer["userid"].(string); !ok {
		self.logger.Error(self.logCat, "No UserID found. Cannot identify user.", nil)
		return "", "", ErrNoUser
	}
	return userid, email, nil
}

func (self *Handler) verifyFxAAssertion(assertion string) (userid, email string, err error) {
    fmt.Printf("Verifying FxA %s\n", assertion)
	if assLen := len(assertion); assLen != len(strings.Map(assertionFilter, assertion)) {
		self.logger.Error(self.logCat, "Assertion contains invalid characters.",
			util.Fields{"assertion": assertion})
		return "", "", ErrAuthorization
	}
/*
    if self.config.GetFlag("auth.disabled") {
        self.logger.Warn(self.logCat, "!!! Skipping validation...", nil)
        return "user1", "user@example.com", nil
    }
*/
    cli := http.Client{}
    args := make(map[string]string)
    args["client_id"] = self.config.Get("oauth.client_id", "invalid")
    args["assertion"] = assertion
    args["state"] = "state"

    argsj, err := json.Marshal(args)
    if err != nil {
        self.logger.Error(self.logCat, "Could not marshal args",
            util.Fields{"error": err.Error()})
        return "", "", err
    }
    validatorUrl := self.config.Get("oauth.endpoint", "oauth.accounts.firefox.com/v1") +
        "/authorization"
    fmt.Printf("### argsj : %s\n", argsj)
    req, err := http.NewRequest("POST", validatorUrl, bytes.NewReader(argsj))
    if err != nil {
        self.logger.Error(self.logCat, "Could not POST verify assertion",
            util.Fields{"error": err.Error()})
        return "", "", err
    }
    req.Header.Add("Content-Type", "application/json")
    res, err := cli.Do(req)
    if err != nil {
        self.logger.Error(self.logCat, "FxA verification failed",
            util.Fields{"error": err.Error()})
        return "", "", err
    }
    buff, err := parseBody(res.Body)
    if err != nil {
        return "", "", err
    }
    if code, ok := buff["code"]; ok && int(code.(float64)) > 299.0 {
        self.logger.Error(self.logCat, "FxA verification failed auth",
            util.Fields{"code": strconv.FormatInt(int64(code.(float64)), 10),
            "error": buff["error"].(string)})
        return "", "", err
    }
    // get the "redirect" url. We're not going to redirect, just get the code.
    if redir, ok := buff["redrirect"]; !ok {
        self.logger.Error(self.logCat, "FxA verification did not return redirect",
            nil)
        return "", "", err
    } else {
        if vurl, err := url.Parse(redir.(string)); err != nil {
            self.logger.Error(self.logCat, "FxA redirect url invalid",
                util.Fields{"error": err.Error(), "url": redir.(string)})
            return "", "", err
        } else {
            code  := vurl.Query().Get("code")
            if len(code) == 0 {
                self.logger.Error(self.logCat, "FxA code not present",
                    util.Fields{"url": redir.(string)})
                return "", "", ErrOauth
            }
            //Convert code to access token.
            accessToken, err := self.getAccessToken(code)
            if err != nil {
                return "", "", ErrOauth
            }
            email, err := self.getUserEmail(accessToken)
            if err != nil {
                return "", "", ErrOauth
            }

           return accessToken, email, nil
        }
    }
}


func (self *Handler) clearSession(sess *sessions.Session) (err error) {
	if sess == nil {
		return
	}
	delete(sess.Values, "userId")
	delete(sess.Values, "deviceId")
	delete(sess.Values, "user")
	delete(sess.Values, "email")
	delete(sess.Values, "accessToken")
	return
}

// get the user id from the session, or the assertion.
func (self *Handler) getUser(resp http.ResponseWriter, req *http.Request) (userid string, user string, err error) {
	session, err := self.session.Get(req, SESSION_NAME)
	if err != nil {
		self.logger.Error(self.logCat, "Could not open session",
			util.Fields{"error": err.Error()})
	} else {
		var ret = false
		if ruid, ok := session.Values["userId"]; ok {
			switch ruid.(type) {
			case string:
				userid = ruid.(string)
				ret = true
			default:
				userid = ""
			}
		}
		if ru, ok := session.Values["user"]; ok {
			switch ru.(type) {
			case string:
				user = ru.(string)
				ret = true
			default:
				user = ""
			}
		}
		if ret {
			return userid, user, nil
		}
	}
	// Nothing in the session,
	var auth string
	if auth = req.FormValue("assertion"); auth == "" {
		return "", "", ErrAuthorization
	}
	userid, user, err = self.verifyFxAAssertion(auth)
	fmt.Printf("### assertion userid: %s\n", userid)
	if err != nil {
		return "", "", ErrAuthorization
	}
	if userid != "" {
		self.clearSession(session)
		session.Save(req, resp)
	}
	return userid, user, nil
}

// set the user info into the session
func (self *Handler) getSessionInfo(resp http.ResponseWriter, req *http.Request, session *sessions.Session) (info *sessionInfoStruct, err error) {
	var userid string
	var user string

	dev := getDevFromUrl(req.URL)
	userid, user, err = self.getUser(resp, req)

	if userid == "" {
		fields := util.Fields{}
		if err != nil {
			fields["error"] = err.Error()
		}
		self.logger.Error("handler", "Could not get user", fields)
		return nil, ErrNoUser
	}
	accesstoken := ""
	if token, ok := session.Values["accessToken"]; ok {
		accesstoken = token.(string)
	}
	info = &sessionInfoStruct{
		UserId:      userid,
		User:        user,
		DeviceId:    dev,
		AccessToken: accesstoken}
	return
}

// log the device's position reply
func (self *Handler) updatePage(devId string, args map[string]interface{}, logPosition bool) (err error) {
	var location storage.Position
	var locked bool

	store, err := storage.Open(self.config, self.logger, self.metrics)
	if err != nil {
		self.logger.Error(self.logCat, "Could not open database",
			util.Fields{"error": err.Error()})
		return err
	}
	defer store.Close()

	for key, arg := range args {
		if len(key) < 2 {
			continue
		}
		switch k := strings.ToLower(key[:2]); k {
		case "la":
			location.Latitude = arg.(float64)
		case "lo":
			location.Longitude = arg.(float64)
		case "al":
			location.Altitude = arg.(float64)
		case "ti":
			location.Time = int64(arg.(float64))
		case "ha":
			locked = isTrue(arg)
			location.Lockable = !locked
			if err = store.SetDeviceLockable(devId, !locked); err != nil {
				return err
			}
		}
	}
	if logPosition {
		if err = store.SetDeviceLocation(devId, location); err != nil {
			return err
		}
		// because go sql locking.
		store.GcPosition(devId)
	}
	if client, ok := Clients[devId]; ok {
		js, _ := json.Marshal(location)
		client.Write(js)
	}
	return nil
}

// log the cmd reply from the device.
func (self *Handler) logReply(devId, cmd string, args replyType) (err error) {
	// verify state and store it

	if v, ok := args["ok"]; !ok {
		return ErrInvalidReply
	} else {
		if !isTrue(v) {
			if e, ok := args["error"]; ok {
				return errors.New(e.(string))
			}
			return errors.New("Unknown error")
		}
		// log the state? (Device is currently cmd-ing)?
		store, err := storage.Open(self.config, self.logger, self.metrics)
		if err != nil {
			self.logger.Error(self.logCat, "Could not open database",
				util.Fields{"error": err.Error()})
			return err
		}
		defer store.Close()
		err = store.Touch(devId)
	}
	return err
}

// Check that a given string intval is within a range.
func (self *Handler) rangeCheck(s string, min, max int64) int64 {
	val, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		self.logger.Warn(self.logCat, "Unparsable range value, returning 0",
			util.Fields{"error": err.Error(),
				"string": s})
		return 0
	}
	if val < min {
		return min
	}
	if val > max {
		return max
	}
	return val
}

//Handler Public Functions

func NewHandler(config *util.MzConfig, logger *util.HekaLogger, metrics *util.Metrics) *Handler {
	store, err := storage.Open(config, logger, metrics)
	if err != nil {
		logger.Error("Handler", "Could not open storage",
			util.Fields{"error": err.Error()})
		return nil
	}
	defer store.Close()

	sessionSecret := config.Get("session.secret", "")
	if sessionSecret == "" {
		logger.Error("Handler", "No session secret defined.", nil)
		return nil
	}
	session := sessions.NewCookieStore([]byte(sessionSecret))
	if err != nil {
		logger.Error("Handler", "Could not open session",
			util.Fields{"error": err.Error()})
		return nil
	}

	// Initialize the data store once. This creates tables and
	// applies required changes.
	store.Init()

	return &Handler{config: config,
		logger:  logger,
		logCat:  "handler",
		session: session,
		metrics: metrics}
}

// Register a new device
func (self *Handler) Register(resp http.ResponseWriter, req *http.Request) {

	var buffer = util.JsMap{}
	var userid string
	var user string
	var pushUrl string
	var deviceid string
	var secret string
	var accepts string
	var lockable bool
	var err error

	self.logCat = "handler:Register"
	resp.Header().Set("Content-Type", "application/json")
	session, err := self.session.Get(req, SESSION_NAME)
	if err != nil {
		self.logger.Error(self.logCat, "Could not open session",
			util.Fields{"error": err.Error()})
		return
	}

	store, err := storage.Open(self.config, self.logger, self.metrics)
	if err != nil {
		self.logger.Error(self.logCat, "Could not open database",
			util.Fields{"error": err.Error()})
		http.Error(resp, "Server error", 500)
		return
	}
	defer store.Close()

	buffer, err = parseBody(req.Body)
    fmt.Printf("### buffer: %+v\n", buffer)
	if err != nil {
		http.Error(resp, "No body", http.StatusBadRequest)
	} else {
		loggedIn := false

		if assertion, ok := buffer["assert"]; ok {
            fmt.Printf("Getting assertion\n")
			userid, user, err = self.verifyFxAAssertion(assertion.(string))
			if err != nil || userid == "" {
				http.Error(resp, "Unauthorized", 401)
				return
			}
			self.logger.Debug(self.logCat, "Got user "+userid, nil)
			user = strings.SplitN(user, "@", 2)[0]
            session.Values["userId"] = userid
			loggedIn = true
		}

		if !loggedIn {
			self.logger.Error(self.logCat, "Not logged in", nil)
			http.Error(resp, "Unauthorized", 401)
			return
		}

		if val, ok := buffer["pushurl"]; !ok || val == nil || len(val.(string)) == 0 {
			self.logger.Error(self.logCat, "Missing SimplePush url", nil)
			http.Error(resp, "Bad Data", 400)
			return
		} else {
			pushUrl = val.(string)
		}
		//ALWAYS generate a new secret on registration!
		secret = GenNonce(16)
		if val, ok := buffer["deviceid"]; !ok || len(val.(string)) == 0 {
			deviceid, err = util.GenUUID4()
		} else {
			deviceid = strings.Map(deviceIdFilter, val.(string))
            if len(deviceid) > 32 {
                deviceid = deviceid[:32]
            }
		}
		if val, ok := buffer["has_passcode"]; !ok {
			lockable = true
		} else {
			lockable, err = strconv.ParseBool(val.(string))
			if err != nil {
				lockable = false
			}
		}
		if val, ok := buffer["accepts"]; ok {
			// collapse the array to a string
			if l := len(val.([]interface{})); l > 0 {
				acc := make([]byte, l)
				for n, ke := range val.([]interface{}) {
					acc[n] = ke.(string)[0]
				}
				accepts = strings.ToLower(string(acc))
			}
		}
		if len(accepts) == 0 {
			accepts = "elrth"
		}

		// create the new device record
		var devId string
		if devId, err = store.RegisterDevice(
			userid,
			storage.Device{
				ID:       deviceid,
				Name:     user,
				Secret:   secret,
				PushUrl:  pushUrl,
				Lockable: lockable,
				Accepts:  accepts,
			}); err != nil {
			self.logger.Error(self.logCat, "Error Registering device", nil)
			http.Error(resp, "Bad Request", 400)
			return
		} else {
			if devId != deviceid {
				self.logger.Error(self.logCat, "Different deviceID returned",
					util.Fields{"original": deviceid, "new": devId})
				http.Error(resp, "Server error", 500)
				return
			}
			self.devId = deviceid
		}
	}
	self.metrics.Increment("device.registration")
	reply, err := json.Marshal(util.Fields{"deviceid": self.devId,
		"secret": secret})
	if err != nil {
		self.logger.Error(self.logCat, "Could not marshal reply",
			util.Fields{"error": err.Error()})
		return
	}
    session.Save(req, resp)
	resp.Write(reply)
	return
}

// Handle the Cmd response from the device and pass next command if available.
func (self *Handler) Cmd(resp http.ResponseWriter, req *http.Request) {
	var err error
	var l int

	self.logCat = "handler:Cmd"
	resp.Header().Set("Content-Type", "application/json")
	store, err := storage.Open(self.config, self.logger, self.metrics)
	if err != nil {
		self.logger.Error(self.logCat, "Could not open database",
			util.Fields{"error": err.Error()})
		http.Error(resp, "Server error", 500)
		return
	}
	defer store.Close()

	deviceId := getDevFromUrl(req.URL)
	if deviceId == "" {
		self.logger.Error(self.logCat, "Invalid call (No device id)", nil)
		http.Error(resp, "Unauthorized", 401)
		return
	}

	devRec, err := store.GetDeviceInfo(deviceId)
	if err != nil {
		switch err {
		case storage.ErrUnknownDevice:
			self.logger.Error(self.logCat,
				"Cmd:Unknown device requesting cmd",
				util.Fields{
					"deviceId": deviceId})
			http.Error(resp, "Unauthorized", 401)
		default:
			self.logger.Error(self.logCat,
				"Cmd:Unhandled Error",
				util.Fields{
					"error":    err.Error(),
					"deviceId": deviceId})
			http.Error(resp, "Unauthorized", 401)
		}
		return
	}
	//decode the body
	var body = make([]byte, req.ContentLength)
	l, err = req.Body.Read(body)
	if err != nil && err != io.EOF {
		self.logger.Error(self.logCat, "Could not read body",
			util.Fields{"error": err.Error()})
		http.Error(resp, "Invalid", 400)
		return
	}
	//validate the Hawk header
	if self.config.GetFlag("hawk.disabled") == false {
		// Remote Hawk
		rhawk := Hawk{logger: self.logger}
		// Local Hawk
		lhawk := Hawk{logger: self.logger}
		// Get the remote signature from the header
		err = rhawk.ParseAuthHeader(req, self.logger)
		if err != nil {
			self.logger.Error(self.logCat, "Could not parse Hawk header",
				util.Fields{"error": err.Error()})
			http.Error(resp, "Unauthorized", 401)
			return
		}

		// Generate the comparator signature from what we know.
		lhawk.Nonce = rhawk.Nonce
		lhawk.Time = rhawk.Time
		//lhawk.Hash = rhawk.Hash

		err = lhawk.GenerateSignature(req, rhawk.Extra, string(body),
			devRec.Secret)
		if err != nil {
			self.logger.Error(self.logCat, "Could not verify sig",
				util.Fields{"error": err.Error()})
			http.Error(resp, "Unauthorized", 401)
			return
		}
		// Do they match?
		if !lhawk.Compare(rhawk.Signature) {
			self.logger.Error(self.logCat, "Cmd:Invalid Hawk Signature",
				util.Fields{
					"expecting": lhawk.Signature,
					"got":       rhawk.Signature,
				})
			http.Error(resp, "Unauthorized", 401)
			return
		}
	}
	// Do the command.
	self.logger.Info(self.logCat, "Handling cmd response from device",
		util.Fields{
			"cmd":    string(body),
			"length": fmt.Sprintf("%d", l),
		})
	// Ignore effectively null commands (e.g. "" or {})
	if l > 2 {
		reply := make(replyType)
		merr := json.Unmarshal(body, &reply)
		//	merr := json.Unmarshal(body, &reply)
		if merr != nil {
			self.logger.Error(self.logCat,
				"Could not unmarshal data",
				util.Fields{
					"error": merr.Error(),
					"body":  string(body)})
			http.Error(resp, "Server Error", 500)
			return
		}

		for cmd, args := range reply {
			c := strings.ToLower(string(cmd[0]))
			if !strings.Contains(devRec.Accepts, c) {
				self.logger.Warn(self.logCat, "Unacceptable Command",
					util.Fields{"unacceptable": c,
						"acceptable": devRec.Accepts})
				continue
			}
			self.metrics.Increment("cmd.received." + string(c))
			switch c {
			case "l", "r", "m", "e":
				err = store.Touch(deviceId)
				self.updatePage(deviceId,
					args.(map[string]interface{}), false)
			case "h":
				argl := make(replyType)
				argl[string(cmd)] = isTrue(args)
				self.updatePage(deviceId, argl, false)
			case "t":
				// track
				err = self.updatePage(deviceId,
					args.(map[string]interface{}), true)
				// store tracking info.
			case "q":
				// User has quit, nuke what we know.
				if self.config.GetFlag("cmd.q.allow") {
					err = store.DeleteDevice(deviceId)
				}
			}
			if err != nil {
				// Log the error
				self.logger.Error(self.logCat, "Error handling command",
					util.Fields{"error": err.Error(),
						"command": string(cmd),
						"device":  deviceId,
						"args":    fmt.Sprintf("%v", args)})
				http.Error(resp,
					"\"Server Error\"",
					http.StatusServiceUnavailable)
				return
			}
		}
	}

	// reply with pending commands
	//
	cmd, err := store.GetPending(deviceId)
	var output = []byte(cmd)
	if err != nil {
		self.logger.Error(self.logCat, "Could not send commands",
			util.Fields{"error": err.Error()})
		http.Error(resp, "\"Server Error\"", http.StatusServiceUnavailable)
	}
	if output == nil || len(output) < 2 {
		output = []byte("{}")
	}
	hawk := Hawk{config: self.config, logger: self.logger}
	authHeader := hawk.AsHeader(req, devRec.ID, string(output),
		"", devRec.Secret)
	resp.Header().Add("Authorization", authHeader)
	// total cheat to get the command without parsing the cmd data.
	if len(cmd) > 2 {
		self.metrics.Increment("cmd.send." + string(cmd[2]))
	}
	resp.Write(output)
}

// Queue the command from the Web Front End for the device.
func (self *Handler) Queue(devRec *storage.Device, cmd string, args, rep *replyType) (status int, err error) {
	status = http.StatusOK

	self.logCat = "handler:Queue"
	deviceId := devRec.ID
	// sanitize values.
	var v interface{}
	var ok bool
	c := strings.ToLower(string(cmd[0]))
	self.logger.Debug(self.logCat, "Processing UI Command",
		util.Fields{"cmd": cmd})
	if !strings.Contains(devRec.Accepts, c) {
		// skip unacceptable command
		self.logger.Warn(self.logCat, "Agent does not accept command",
			util.Fields{"unacceptable": c,
				"acceptable": devRec.Accepts})
		(*rep)["error"] = 422
		(*rep)["cmd"] = cmd
		return
	}
	rargs := *args
	switch c {
	case "l":
		if v, ok = rargs["c"]; ok {
			max, err := strconv.ParseInt(self.config.Get("cmd.c.max", "9999"),
				10, 64)
			if err != nil {
				max = 9999
			}
			vs := v.(string)
			rargs["c"] = self.rangeCheck(
				strings.Map(digitsOnly, vs[:minInt(4, len(vs))]),
				0,
				max)
		}
		if v, ok = rargs["m"]; ok {
			vs := v.(string)
			rargs["m"] = strings.Map(asciiOnly,
				vs[:minInt(100, len(vs))])
		}
	case "r", "t":
		if v, ok = rargs["d"]; ok {
			vs := ""
			max, err := strconv.ParseInt(
				self.config.Get("cmd."+c+".max",
					"10500"), 10, 64)
			if err != nil {
				max = 10500
			}
			switch v.(type) {
			case string:
				vs = v.(string)
			case float64:
				vs = strconv.FormatFloat(v.(float64), 'f', 0, 64)
			default:
				vs = fmt.Sprintf("%s", v)
			}
			rargs["d"] = self.rangeCheck(
				strings.Map(digitsOnly, vs),
				0,
				max)
		}
	case "e":
		rargs = replyType{}
	default:
		self.logger.Warn(self.logCat, "Invalid Command",
			util.Fields{"command": string(cmd),
				"device": deviceId,
				"args":   fmt.Sprintf("%v", rargs)})
		return http.StatusBadRequest, errors.New("\"Invalid Command\"")
	}
	fixed, err := json.Marshal(storage.Unstructured{c: rargs})
	if err != nil {
		// Log the error
		self.logger.Error(self.logCat, "Error handling command",
			util.Fields{"error": err.Error(),
				"command": string(cmd),
				"device":  deviceId,
				"args":    fmt.Sprintf("%v", rargs)})
		return http.StatusServiceUnavailable, errors.New("\"Server Error\"")
	}

	store, err := storage.Open(self.config, self.logger, self.metrics)
	if err != nil {
		self.logger.Error(self.logCat, "Could not open database",
			util.Fields{"error": err.Error()})
		return http.StatusServiceUnavailable, errors.New("\"Server Error\"")
	}
	defer store.Close()

	err = store.StoreCommand(deviceId, string(fixed))
	if err != nil {
		// Log the error
		self.logger.Error(self.logCat, "Error storing command",
			util.Fields{"error": err.Error(),
				"command": string(cmd),
				"device":  deviceId,
				"args":    fmt.Sprintf("%v", args)})
		return http.StatusServiceUnavailable, errors.New("\"Server Error\"")
	}
	// trigger the push
	self.metrics.Increment("cmd.store." + c)
	self.metrics.Increment("push.send")
	err = SendPush(devRec, self.config)
	if err != nil {
		self.logger.Error(self.logCat, "Could not send Push",
			util.Fields{"error": err.Error(),
				"pushUrl": devRec.PushUrl})
		return http.StatusServiceUnavailable, errors.New("\"Server Error\"")
	}
	return
}

// Accept a command to queue from the REST interface
func (self *Handler) RestQueue(resp http.ResponseWriter, req *http.Request) {
	/* Queue commands for the device.
	 */
	var err error
	var lbody int

	resp.Header().Set("Content-Type", "application/json")
	rep := make(replyType)
	self.logCat = "handler:Queue"

	session, err := self.session.Get(req, SESSION_NAME)
	if err != nil {
		self.logger.Error(self.logCat, "Unauthorized access to Cmd",
			util.Fields{"error": err.Error()})
		http.Error(resp, "Unauthorized", 401)
		return
	}
	store, err := storage.Open(self.config, self.logger, self.metrics)
	if err != nil {
		self.logger.Error(self.logCat, "Could not open database",
			util.Fields{"error": err.Error()})
		http.Error(resp, "Server error", 500)
		return
	}
	defer store.Close()

	deviceId := getDevFromUrl(req.URL)
	if deviceId == "" {
		self.logger.Error(self.logCat, "Invalid call (No device id)", nil)
		http.Error(resp, "Unauthorized", 401)
		return
	}
	userId, _, err := self.getUser(resp, req)
	if userId == "" || err != nil {
		self.logger.Error(self.logCat, "No userid", nil)
		self.clearSession(session)
		session.Save(req, resp)
		http.Error(resp, "Unauthorized", 401)
		return
	}

	devRec, err := store.GetDeviceInfo(deviceId)
	if err != nil || devRec == nil {
		fields := util.Fields{"deviceId": deviceId}
		if err != nil {
			fields["error"] = err.Error()
		}
		if devRec != nil {
			fields["devRec"] = devRec.ID
		}
		self.logger.Error(self.logCat, "Could not get userid", fields)
		self.clearSession(session)
		session.Save(req, resp)
		http.Error(resp, "Unauthorized", 401)
		return
	}
	if devRec.User != userId {
		self.logger.Error(self.logCat, "Unauthorized device",
			util.Fields{"devrec": devRec.User,
				"userid": userId})
		self.clearSession(session)
		session.Save(req, resp)
		http.Error(resp, "Unauthorized", 401)
		return
	}
	if devRec == nil {
		self.logger.Error(self.logCat,
			"Queue:User requested unknown device",
			util.Fields{
				"deviceId": deviceId,
				"userId":   userId})
		self.clearSession(session)
		session.Save(req, resp)
		http.Error(resp, "Unauthorized", 401)
		return
	}
	if err != nil {
		switch err {
		default:
			self.logger.Error(self.logCat,
				"Cmd:Unhandled Error",
				util.Fields{
					"error":    err.Error(),
					"deviceId": deviceId})
			http.Error(resp, "Unauthorized", 401)
		}
		return
	}

	//decode the body
	var body = make([]byte, req.ContentLength)
	lbody, err = req.Body.Read(body)
	if err != nil && err != io.EOF {
		self.logger.Error(self.logCat, "Could not read body",
			util.Fields{"error": err.Error()})
		http.Error(resp, "Invalid", 400)
		return
	}
	self.logger.Info(self.logCat, "Handling cmd from UI",
		util.Fields{
			"cmd":    string(body),
			"length": fmt.Sprintf("%d", lbody),
		})
	if lbody > 0 {
		reply := make(replyType)
		merr := json.Unmarshal(body, &reply)
		//	merr := json.Unmarshal(body, &reply)
		if merr != nil {
			self.logger.Error(self.logCat, "Could not unmarshal data",
				util.Fields{
					"error": merr.Error(),
					"body":  string(body)})
			http.Error(resp, "Server Error", 500)
			return
		}

		for cmd, args := range reply {
			rargs := replyType(args.(map[string]interface{}))
			status, err := self.Queue(devRec, cmd, &rargs, &rep)
			if err != nil {
				self.logger.Error(self.logCat, "Error processing command",
					util.Fields{
						"error": err.Error(),
						"cmd":   cmd,
						"args":  fmt.Sprintf("%+v", args)})
				http.Error(resp, err.Error(), status)
				return
			}
		}
	}
	repl, _ := json.Marshal(rep)
	self.metrics.Increment("cmd.queued.rest")
	resp.Write(repl)
}

// user login functions
func (self *Handler) Index(resp http.ResponseWriter, req *http.Request) {
	/* Handle a user login to the web UI
	 */

	self.logCat = "handler:Index"
	// This should be handled by an nginx rule.

	if strings.Contains(req.URL.Path, "/static/") {
		self.Static(resp, req)
		return
	}
	var data struct {
		ProductName string
		UserId      string
		MapKey      string
		DeviceList  []storage.DeviceList
		Device      *storage.Device
		Host        map[string]string
	}
	var err error

	session, err := self.session.Get(req, SESSION_NAME)
	if err != nil {
		self.logger.Error(self.logCat, "Could not initialize session",
			util.Fields{"error": err.Error()})
	}

	store, err := storage.Open(self.config, self.logger, self.metrics)
	if err != nil {
		self.logger.Error(self.logCat, "Could not open database",
			util.Fields{"error": err.Error()})
		http.Error(resp, "Server error", 500)
		return
	}
	defer store.Close()

	// Get this from the config file?
	data.ProductName = self.config.Get("productname", "Find My Device")

	data.MapKey = self.config.Get("mapbox.key", "")

	// host information (for websocket callback)
	data.Host = make(map[string]string)
	data.Host["Hostname"] = self.config.Get("ws_hostname", "localhost")
	data.Host["Client_id"] = self.config.Get("oauth.client_id", "none")
	data.Host["Endpoint"] = self.config.Get("oauth.endpoint", "https://oauth.accounts.firefox.com/v1")
	// TODO: generate "state" code thingy
	data.Host["State"] = "somestate"

	// get the cached session info (if present)
	// will also resolve assertions and other bits to get user and dev info.
	sessionInfo, err := self.getSessionInfo(resp, req, session)
	if err == nil {
		// we have user info, use it.
		data.UserId = sessionInfo.UserId
		if sessionInfo.DeviceId == "" {
			data.DeviceList, err = store.GetDevicesForUser(data.UserId)
			if err != nil {
				self.logger.Error(self.logCat, "Could not get user devices",
					util.Fields{"error": err.Error(),
						"user": data.UserId})
			}
			if len(data.DeviceList) == 1 {
				sessionInfo.DeviceId = (data.DeviceList[0]).ID
				data.DeviceList = nil
			}
		}
		if sessionInfo.DeviceId != "" {
			data.Device, err = store.GetDeviceInfo(sessionInfo.DeviceId)
			if err != nil {
				self.logger.Error(self.logCat, "Could not get device info",
					util.Fields{"error": err.Error(),
						"user": data.UserId})
				if file, err := ioutil.ReadFile("static/error.html"); err == nil {
					resp.Write(file)
				}
				return
			}
			data.Device.PreviousPositions, err = store.GetPositions(sessionInfo.DeviceId)
			if err != nil {
				self.logger.Error(self.logCat,
					"Could not get device's position information",
					util.Fields{"error": err.Error(),
						"userId": data.UserId,
						"user":   sessionInfo.User,
						"device": sessionInfo.DeviceId})
				return
			}
		}
	}

	// render the page from the template
	tmpl, err := template.New("index.html").ParseFiles("static/index.html")
	if err != nil {
		self.logger.Error(self.logCat, "Could not display index page",
			util.Fields{"error": err.Error(),
				"user": data.UserId})
		if file, err := ioutil.ReadFile("static/error.html"); err == nil {
			resp.Write(file)
		}
		return
	}
	if sessionInfo != nil {
		session.Values["userId"] = sessionInfo.UserId
		session.Values["user"] = sessionInfo.User
		session.Values["deviceId"] = sessionInfo.DeviceId
	} else {
		if !session.IsNew {
			self.clearSession(session)
			session.Save(req, resp)
		}
	}
	err = tmpl.Execute(resp, data)
	if err != nil {
		self.logger.Error(self.logCat, "Could not execute template",
			util.Fields{"error": err.Error()})
	}
	self.metrics.Increment("page.index")
	return
}

// Show the state of the user's devices.
func (self *Handler) State(resp http.ResponseWriter, req *http.Request) {
	// get session info
	self.logCat = "handler:State"

	resp.Header().Set("Content-Type", "application/json")

	store, err := storage.Open(self.config, self.logger, self.metrics)
	if err != nil {
		self.logger.Error(self.logCat, "Could not open database",
			util.Fields{"error": err.Error()})
		http.Error(resp, "Server error", 500)
		return
	}
	defer store.Close()

	session, err := self.session.Get(req, SESSION_NAME)
	if err != nil {
		self.logger.Error(self.logCat, "Could not get session info",
			util.Fields{"error": err.Error()})
		http.Error(resp, err.Error(), 500)
	}
	sessionInfo, err := self.getSessionInfo(resp, req, session)
	if err != nil && err != ErrNoUser {
		http.Error(resp, err.Error(), 500)
		return
	}
	devInfo, err := store.GetDeviceInfo(sessionInfo.DeviceId)
	if err != nil {
		http.Error(resp, err.Error(), 500)
		return
	}
	// add the user session cookie
	if sessionInfo != nil {
		session.Values["userId"] = sessionInfo.UserId
		session.Values["deviceId"] = sessionInfo.DeviceId
	}
	session.Save(req, resp)
	// display the device info...
	reply, err := json.Marshal(devInfo)
	if err == nil {
		resp.Write([]byte(reply))
	}
}

// Show the status of the program (For Load Balancers)
func (self *Handler) Status(resp http.ResponseWriter, req *http.Request) {
	self.logCat = "handler:Status"

	resp.Header().Set("Content-Type", "application/json")
	reply := replyType{
		"status":     "ok",
		"goroutines": runtime.NumGoroutine(),
		"version":    req.URL.Path[len("/status/"):],
	}
	rep, _ := json.Marshal(reply)
	resp.Write(rep)
}

// Handle requests for static content (should be an NGINX rule)
func (self *Handler) Static(resp http.ResponseWriter, req *http.Request) {
	/* This should be handled by something like an nginx rule
	 */

	if !self.config.GetFlag("use_insecure_static") {
		return
	}
	sl := len("/static/")
	if len(req.URL.Path) > sl {
		http.ServeFile(resp, req, "./static/"+req.URL.Path[sl:])
	}
}

// Display the current metrics as a JSON snapshot
func (self *Handler) Metrics(resp http.ResponseWriter, req *http.Request) {
	snapshot := self.metrics.Snapshot()

	resp.Header().Set("Content-Type", "application/json")
	reply, err := json.Marshal(snapshot)
	if err != nil {
		self.logger.Error(self.logCat, "Could not generate metrics report",
			util.Fields{"error": err.Error()})
		resp.Write([]byte("{}"))
		return
	}
	if reply == nil {
		reply = []byte("{}")
	}
	resp.Write(reply)
}

func (self *Handler) getAccessToken(code string) (accessToken string, err error) {
	token_url := self.config.Get("oauth.endpoint", OAUTH_ENDPOINT) + "/token"
	vals := make(map[string]string)
	vals["client_id"] = self.config.Get("oauth.client_id", "invalid")
	vals["client_secret"] = self.config.Get("oauth.client_secret", "invalid")
	vals["code"] = code
	vd, err := json.Marshal(vals)
	fmt.Printf("### %s\n", string(vd))
	if err != nil {
		self.logger.Error(self.logCat, "Could not marshal vals to json",
			util.Fields{"error": err.Error()})
		return "", err
	}
	token_resp, err := http.Post(token_url, "application/json", bytes.NewBuffer(vd))
	if err != nil {
		self.logger.Error(self.logCat, "Could not get oauth token",
			util.Fields{"code": code, "error": err.Error()})
		return "", ErrOauth
	}
	reply, err := parseBody(token_resp.Body)
	fmt.Printf("### %+v\n", reply)
	token, ok := reply["access_token"]
	if !ok {
		self.logger.Error(self.logCat, "OAuth Access token missing from reply",
			util.Fields{"code": code})
		return "", ErrOauth
	}
	return token.(string), nil
}

func (self *Handler) getUserEmail(accessToken string) (email string, err error) {
	client := &http.Client{}
	req, err := http.NewRequest("POST",
		self.config.Get("fxa.content.endpoint", CONTENT_ENDPOINT)+"/email",
		nil)
	if err != nil {
		self.logger.Error(self.logCat, "Could not POST to get email",
			util.Fields{"error": err.Error()})
		return "", err
	}
	req.Header.Add("Authorization", "Bearer "+accessToken)
	resp, err := client.Do(req)
	if err != nil {
		self.logger.Error(self.logCat, "Could not get user email",
			util.Fields{"error": err.Error()})
		return "", err
	}
	buffer, err := parseBody(resp.Body)
	if err != nil {
		self.logger.Error(self.logCat, "Could not parse body",
			util.Fields{"error": err.Error()})
		return "", err
	}
	if _, ok := buffer["email"]; !ok {
		self.logger.Error(self.logCat, "Response did not contain email",
			nil)
		return "", ErrNoUser
	}
	return buffer["email"].(string), nil
}

func (self *Handler) genHash(input string) (output string) {
	hasher := sha256.New()
	hasher.Write([]byte(input))
	return hex.EncodeToString(hasher.Sum(nil))
}
func (self *Handler) OAuthCallback(resp http.ResponseWriter, req *http.Request) {
	self.logCat = "oauth"

	session, err := self.session.Get(req, SESSION_NAME)
	fmt.Printf("### oauth session: %s, err: %s\n", session, err)
	if err == nil {
		if _, ok := session.Values["accessToken"]; !ok {
			// get the "state", and "code"
			state := req.FormValue("state")
			code := req.FormValue("code")
			// TODO: check "state" matches magic code thingy
			if state == "" {
				self.logger.Error(self.logCat, "No State", nil)
				return
			}
			if code == "" {
				self.logger.Error(self.logCat, "Missing code value", nil)
				http.Error(resp, "Unauthorized", 401)
				return
			}

			// fetch the token:
			fmt.Printf("### Getting access token\n")
			token, err := self.getAccessToken(code)
			if err != nil {
				self.logger.Error(self.logCat, "Could not get access token",
					util.Fields{"error": err.Error()})
				http.Error(resp, "Unauthorized", 401)
				return
			}
			fmt.Printf("### store user token %s\n", token)
            delete(session.Values, "email")
			session.Values["accessToken"] = token
        }
        if _, ok := session.Values["email"]; !ok {
			email, err := self.getUserEmail(session.Values["accessToken"].(string))
			if err != nil {
				self.logger.Error(self.logCat, "Could not get email",
					util.Fields{"error": err.Error()})
				http.Error(resp, "Unauthorized", 401)
				return
			}
			session.Values["email"] = email
			session.Save(req, resp)
		}
	}
	self.Index(resp, req)
	return
}

// Add a new trackable client.
func addClient(id string, sock *WWS) {
	defer muClient.Unlock()
	muClient.Lock()
	Clients[id] = sock
}

// remove a trackable client
func rmClient(id string) {
	defer muClient.Unlock()
	muClient.Lock()
	if _, ok := Clients[id]; ok {
		delete(Clients, id)
	}
}

// Handle Websocket processing.
func (self *Handler) WSSocketHandler(ws *websocket.Conn) {
	self.logCat = "handler:Socket"
	store, err := storage.Open(self.config, self.logger, self.metrics)
	if err != nil {
		self.logger.Error(self.logCat, "Could not open database",
			util.Fields{"error": err.Error()})
		return
	}
	defer store.Close()

	self.devId = getDevFromUrl(ws.Request().URL)
	devRec, err := store.GetDeviceInfo(self.devId)
	if err != nil {
		self.logger.Error(self.logCat, "Invalid Device for socket",
			util.Fields{"error": err.Error(),
				"devId": self.devId})
		return
	}

	sock := &WWS{
		Socket:  ws,
		Handler: self,
		Device:  devRec,
		Logger:  self.logger,
		Born:    time.Now(),
		Quit:    false}

	defer func(logger *util.HekaLogger) {
		if r := recover(); r != nil {
			debug.PrintStack()
			if logger != nil {
				logger.Error(self.logCat, "Uknown Error",
					util.Fields{"error": r.(error).Error()})
			} else {
				log.Printf("Socket Unknown Error: %s\n", r.(error).Error())
			}
		}
	}(sock.Logger)

	// get the device id from the localAddress:
	Url, err := url.Parse(ws.LocalAddr().String())
	if err != nil {
		self.logger.Error(self.logCat, "Unparsable URL for websocket",
			util.Fields{"error": err.Error()})
		return
	}
	elements := strings.Split(Url.Path, "/")
	var deviceId string
	if len(elements) < 3 {
		self.logger.Error(self.logCat, "No deviceID found",
			util.Fields{"error": err.Error(),
				"path": Url.Path})
		return
	}
	deviceId = elements[3]
	// Kill the old client.
	if client, ok := Clients[deviceId]; ok {
		client.Quit = true
	}

	self.metrics.Increment("page.socket")
	addClient(deviceId, sock)
	sock.Run()
	self.metrics.Decrement("page.socket")
	self.metrics.Timer("page.socket", time.Now().Unix()-sock.Born.Unix())
	rmClient(deviceId)
}
