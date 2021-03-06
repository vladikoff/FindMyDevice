package wmf

/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

import (
	"github.com/mozilla-services/FindMyDevice/util"

	"bytes"
	"encoding/json"
	"io"
	"io/ioutil"
	"net/url"
	"strconv"
	"strings"

	//	"fmt"
)

//filters
func digitsOnly(r rune) rune {
	switch {
	case r >= '0' && r <= '9':
		return r
	default:
		return -1
	}
}

func asciiOnly(r rune) rune {
	switch {
	case r >= 32 && r <= 255:
		return r
	default:
		return -1
	}
}

func deviceIdFilter(r rune) rune {
	if bytes.IndexRune([]byte("ABCDEFabcdef0123456789-"), r) < 0 {
		return rune(-1)
	}
	return r
}

func assertionFilter(r rune) rune {
	// wish that base64.go exported this publicly:
	if bytes.IndexRune([]byte("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_~.="), r) < 0 {
		return rune(-1)
	}
	return r
}

// parse a body and return the JSON
func parseBody(rbody io.ReadCloser) (rep util.JsMap, raw string, err error) {
	var body []byte
	rep = util.JsMap{}
	defer rbody.Close()
	if body, err = ioutil.ReadAll(rbody.(io.Reader)); err != nil {
		return nil, "", err
	}
	// fmt.Printf("### parseBody: %s\n", body)
	if err = json.Unmarshal(body, &rep); err != nil {
		return nil, string(body), err
	}
	return rep, string(body), nil
}

// Take an interface value and return if it's true or not.
func isTrue(val interface{}) bool {
	switch val.(type) {
	case string:
		flag, _ := strconv.ParseBool(val.(string))
		return flag
	case bool:
		return val.(bool)
	case int64:
		return val.(int64) != 0
	default:
		return false
	}
}

// There's no built in min function.
// awesome.
func minInt(x, y int) int {
	if x < y {
		return x
	}
	return y
}

//filter
// get the device id from the URL path
func getDevFromUrl(u *url.URL) (devId string) {
	if len(u.Path) < 10 || !strings.Contains(u.Path, "/") {
		return ""
	}
	elements := strings.Split(strings.TrimRight(u.Path, "/"), "/")
	devId = strings.Map(deviceIdFilter, elements[len(elements)-1])
	if len(devId) > 32 {
		devId = devId[:32]
	}
	return devId
}
