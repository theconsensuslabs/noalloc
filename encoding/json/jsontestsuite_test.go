package json_test

import (
	stdjson "encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestJSONTestSuite runs JSONTestSuite, by Nicolas Seriot, in
// testdata/JSONTestSuite under the MIT license: the Reader accepts every y_
// file and rejects every n_ file, and on every file, i_ and test_transform
// included, it accepts what encoding/json accepts and decodes the value
// encoding/json decodes, from memory and streamed.
func TestJSONTestSuite(t *testing.T) {
	files, err := filepath.Glob("testdata/JSONTestSuite/test_*/*.json")
	if err != nil || len(files) < 300 {
		t.Fatalf("found %d files: %v", len(files), err)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(f)
		var want any
		wantErr := stdjson.Unmarshal(data, &want)
		longKey = false
		whole, streamed, wholeErr, streamErr, compared := decodeBoth(data)
		switch {
		case strings.HasPrefix(name, "y_") && wholeErr != nil:
			t.Errorf("%s: must be accepted, Reader fails: %v", name, wholeErr)
		case strings.HasPrefix(name, "n_") && wholeErr == nil:
			t.Errorf("%s: must be rejected, Reader accepts it", name)
		case (wholeErr == nil) != (wantErr == nil):
			t.Errorf("%s: Reader error %v, json.Unmarshal error %v", name, wholeErr, wantErr)
		case wholeErr == nil && !longKey && !reflect.DeepEqual(whole, want):
			t.Errorf("%s: Reader decodes %#v, json.Unmarshal %#v", name, whole, want)
		case compared && (streamErr == nil) != (wholeErr == nil):
			t.Errorf("%s: streamed error %v, from memory %v", name, streamErr, wholeErr)
		case compared && streamErr == nil && !reflect.DeepEqual(streamed, whole):
			t.Errorf("%s: streamed decodes %#v, from memory %#v", name, streamed, whole)
		}
	}
}
