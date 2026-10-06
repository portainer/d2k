// Adapted from github.com/moby/moby api/types/filters/parse.go (Apache License 2.0,
// Copyright Docker, Inc.; see https://github.com/moby/moby/blob/master/LICENSE).

/*
Package filters provides tools for encoding a mapping of keys to a set of
multiple values.
*/
package filters

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Args stores a mapping of keys to a set of multiple values.
type Args struct {
	fields map[string]map[string]bool
}

// KeyValuePair are used to initialize a new Args
type KeyValuePair struct {
	Key   string
	Value string
}

// Arg creates a new KeyValuePair for initializing Args
func Arg(key, value string) KeyValuePair {
	return KeyValuePair{Key: key, Value: value}
}

// NewArgs returns a new Args populated with the initial args
func NewArgs(initialArgs ...KeyValuePair) Args {
	args := Args{fields: map[string]map[string]bool{}}
	for _, arg := range initialArgs {
		args.Add(arg.Key, arg.Value)
	}
	return args
}

// FromJSON decodes a JSON encoded string into Args
func FromJSON(p string) (Args, error) {
	args := NewArgs()

	if p == "" {
		return args, nil
	}

	raw := []byte(p)
	err := json.Unmarshal(raw, &args)
	if err == nil {
		return args, nil
	}

	// Fallback to parsing arguments in the legacy slice format
	deprecated := map[string][]string{}
	if legacyErr := json.Unmarshal(raw, &deprecated); legacyErr != nil {
		return args, &invalidFilter{}
	}

	args.fields = deprecatedArgs(deprecated)
	return args, nil
}

// UnmarshalJSON populates the Args from JSON encode bytes
func (args Args) UnmarshalJSON(raw []byte) error {
	return json.Unmarshal(raw, &args.fields)
}

// Get returns the list of values associated with the key
func (args Args) Get(key string) []string {
	values := args.fields[key]
	if values == nil {
		return make([]string, 0)
	}
	slice := make([]string, 0, len(values))
	for key := range values {
		slice = append(slice, key)
	}
	return slice
}

// Add a new value to the set of values
func (args Args) Add(key, value string) {
	if _, ok := args.fields[key]; ok {
		args.fields[key][value] = true
	} else {
		args.fields[key] = map[string]bool{value: true}
	}
}

// MatchKVList returns true if all the pairs in sources exist as key=value
// pairs in the mapping at key, or if there are no values at key.
func (args Args) MatchKVList(key string, sources map[string]string) bool {
	fieldValues := args.fields[key]

	// do not filter if there is no filter set or cannot determine filter
	if len(fieldValues) == 0 {
		return true
	}

	if len(sources) == 0 {
		return false
	}

	for value := range fieldValues {
		testK, testV, hasValue := strings.Cut(value, "=")

		v, ok := sources[testK]
		if !ok {
			return false
		}
		if hasValue && testV != v {
			return false
		}
	}

	return true
}

// Match returns true if any of the values at key match the source string
func (args Args) Match(field, source string) bool {
	if args.ExactMatch(field, source) {
		return true
	}

	fieldValues := args.fields[field]
	for name2match := range fieldValues {
		match, err := regexp.MatchString(name2match, source)
		if err != nil {
			continue
		}
		if match {
			return true
		}
	}
	return false
}

// ExactMatch returns true if the source matches exactly one of the values.
func (args Args) ExactMatch(key, source string) bool {
	fieldValues, ok := args.fields[key]
	// do not filter if there is no filter set or cannot determine filter
	if !ok || len(fieldValues) == 0 {
		return true
	}

	// try to match full name value to avoid O(N) regular expression matching
	return fieldValues[source]
}

// Contains returns true if the key exists in the mapping
func (args Args) Contains(field string) bool {
	_, ok := args.fields[field]
	return ok
}

// GetBoolOrDefault returns a boolean value of the key if the key is present
// and is interpretable as a boolean value. Otherwise the default value is returned.
// Error is not nil only if the filter values are not valid boolean or are conflicting.
func (args Args) GetBoolOrDefault(key string, defaultValue bool) (bool, error) {
	fieldValues, ok := args.fields[key]
	if !ok {
		return defaultValue, nil
	}

	if len(fieldValues) == 0 {
		return defaultValue, &invalidFilter{key, nil}
	}

	isFalse := fieldValues["0"] || fieldValues["false"]
	isTrue := fieldValues["1"] || fieldValues["true"]
	if isFalse == isTrue {
		// Either no or conflicting truthy/falsy value were provided
		return defaultValue, &invalidFilter{key, args.Get(key)}
	}
	return isTrue, nil
}

// Validate compared the set of accepted keys against the keys in the mapping.
// An error is returned if any mapping keys are not in the accepted set.
func (args Args) Validate(accepted map[string]bool) error {
	for name := range args.fields {
		if !accepted[name] {
			return &invalidFilter{name, nil}
		}
	}
	return nil
}

func deprecatedArgs(d map[string][]string) map[string]map[string]bool {
	m := map[string]map[string]bool{}
	for k, v := range d {
		values := map[string]bool{}
		for _, vv := range v {
			values[vv] = true
		}
		m[k] = values
	}
	return m
}
