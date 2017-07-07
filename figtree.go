package figtree

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/pkg/errors"

	yaml "gopkg.in/coryb/yaml.v2"
	logging "gopkg.in/op/go-logging.v1"
)

var log = logging.MustGetLogger("figtree")

func LoadConfig(configFile string, options interface{}) error {
	// assert options is a pointer

	paths := findParentPaths(configFile)
	paths = append([]string{fmt.Sprintf("/etc/%s", configFile)}, paths...)

	basePath, err := os.Getwd()
	if err != nil {
		return err
	}

	// iterate paths in reverse
	for i := len(paths) - 1; i >= 0; i-- {
		file := paths[i]
		rel, err := filepath.Rel(basePath, file)
		if err != nil {
			rel = file
		}
		m := &merger{sourceFile: rel}
		type tmpOpts struct {
			Config ConfigOptions
		}

		if stat, err := os.Stat(file); err == nil {
			tmp := reflect.New(reflect.ValueOf(options).Elem().Type()).Interface()
			if stat.Mode()&0111 == 0 {
				log.Debugf("Loading config %s", file)
				// first parse out any config processing option
				if data, err := ioutil.ReadFile(file); err == nil {
					err := yaml.Unmarshal(data, m)
					if err != nil {
						return errors.Wrap(err, fmt.Sprintf("Unable to parse %s", file))
					}

					err = yaml.Unmarshal(data, tmp)
					if err != nil {
						return errors.Wrap(err, fmt.Sprintf("Unable to parse %s", file))
					}
					// if reflect.ValueOf(tmp).Kind() == reflect.Map {
					// 	tmp, _ = util.YamlFixup(tmp)
					// }
				}
			} else {
				log.Debugf("Found Executable Config file: %s", file)
				// it is executable, so run it and try to parse the output
				cmd := exec.Command(file)
				stdout := bytes.NewBufferString("")
				cmd.Stdout = stdout
				cmd.Stderr = bytes.NewBufferString("")
				if err := cmd.Run(); err != nil {
					return errors.Wrap(err, fmt.Sprintf("%s is exectuable, but it failed to execute:\n%s", file, cmd.Stderr))
				}
				// first parse out any config processing option
				err := yaml.Unmarshal(stdout.Bytes(), m)
				if err != nil {
					return errors.Wrap(err, fmt.Sprintf("Unable to parse %s", file))
				}
				err = yaml.Unmarshal(stdout.Bytes(), tmp)
				if err != nil {
					return errors.Wrap(err, fmt.Sprintf("Failed to parse STDOUT from executable config file %s", file))
				}
			}
			m.setSource(reflect.ValueOf(tmp))
			m.mergeStructs(
				reflect.ValueOf(options),
				reflect.ValueOf(tmp),
			)
			if m.Config.Stop {
				break
			}
		}
	}
	return nil
}

type ConfigOptions struct {
	Overwrite []string `json:"overwrite,omitempty" yaml:"overwrite,omitempty"`
	Stop      bool     `json:"stop,omitempty" yaml:"stop,omitempty"`
	// Merge     bool     `json:"merge,omitempty" yaml:"merge,omitempty"`
}

type merger struct {
	sourceFile string
	Config     ConfigOptions `json:"config,omitempty" yaml:"config,omitempty"`
}

func yamlFieldName(sf reflect.StructField) string {
	if tag, ok := sf.Tag.Lookup("yaml"); ok {
		// with yaml:"foobar,omitempty"
		// we just want to the "foobar" part
		parts := strings.Split(tag, ",")
		return parts[0]
	}
	return sf.Name
}

func (m *merger) mustOverwrite(name string) bool {
	for _, prop := range m.Config.Overwrite {
		if name == prop {
			return true
		}
	}
	return false
}

func (m *merger) isEmpty(v reflect.Value) bool {
	return reflect.DeepEqual(v.Interface(), reflect.Zero(v.Type()).Interface())
}

func (m *merger) isSame(v1, v2 reflect.Value) bool {
	return reflect.DeepEqual(v1.Interface(), v2.Interface())
}

// recursively set the Source attribute of the Options
func (m *merger) setSource(v reflect.Value) {
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Map:
		for _, key := range v.MapKeys() {
			keyval := v.MapIndex(key)
			if keyval.Kind() == reflect.Struct && keyval.FieldByName("Source").IsValid() {
				// map values are immutable, so we need to copy the value
				// update the value, then re-insert the value to the map
				newval := reflect.New(keyval.Type())
				newval.Elem().Set(keyval)
				m.setSource(newval)
				v.SetMapIndex(key, newval.Elem())
			}
		}
	case reflect.Struct:
		if v.CanAddr() {
			if option, ok := v.Addr().Interface().(Option); ok {
				if option.IsDefined() {
					option.SetSource(m.sourceFile)
				}
				return
			}
		}
		for i := 0; i < v.NumField(); i++ {
			m.setSource(v.Field(i))
		}
	case reflect.Array:
		fallthrough
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			m.setSource(v.Index(i))
		}
	}
}

func (m *merger) mergeStructs(ov, nv reflect.Value) {
	if ov.Kind() == reflect.Ptr {
		ov = ov.Elem()
	}
	if nv.Kind() == reflect.Ptr {
		nv = nv.Elem()
	}
	if ov.Kind() == reflect.Map && nv.Kind() == reflect.Map {
		m.mergeMaps(ov, nv)
		return
	}
	if !ov.IsValid() || !nv.IsValid() {
		return
	}
	for i := 0; i < nv.NumField(); i++ {
		fieldName := yamlFieldName(ov.Type().Field(i))

		if (m.isEmpty(ov.Field(i)) || m.mustOverwrite(fieldName)) && !m.isSame(ov.Field(i), nv.Field(i)) {
			log.Debugf("Setting %s to %#v", nv.Type().Field(i).Name, nv.Field(i).Interface())
			ov.Field(i).Set(nv.Field(i))
		} else {
			switch ov.Field(i).Kind() {
			case reflect.Map:
				if nv.Field(i).Len() > 0 {
					log.Debugf("Merging: %v with %v", ov.Field(i), nv.Field(i))
					m.mergeMaps(ov.Field(i), nv.Field(i))
				}
			case reflect.Slice:
				if nv.Field(i).Len() > 0 {
					log.Debugf("Merging: %v with %v", ov.Field(i), nv.Field(i))
					if ov.Field(i).CanSet() {
						if ov.Field(i).Len() == 0 {
							ov.Field(i).Set(nv.Field(i))
						} else {
							log.Debugf("Merging: %v with %v", ov.Field(i), nv.Field(i))
							ov.Field(i).Set(m.mergeArrays(ov.Field(i), nv.Field(i)))
						}
					}

				}
			case reflect.Array:
				if nv.Field(i).Len() > 0 {
					log.Debugf("Merging: %v with %v", ov.Field(i), nv.Field(i))
					ov.Field(i).Set(m.mergeArrays(ov.Field(i), nv.Field(i)))
				}
			}
		}
	}
}

func (m *merger) mergeMaps(ov, nv reflect.Value) {
	for _, key := range nv.MapKeys() {
		if !ov.MapIndex(key).IsValid() {
			log.Debugf("Setting %v to %#v", key.Interface(), nv.MapIndex(key).Interface())
			ov.SetMapIndex(key, nv.MapIndex(key))
		} else {
			ovi := reflect.ValueOf(ov.MapIndex(key).Interface())
			nvi := reflect.ValueOf(nv.MapIndex(key).Interface())
			switch ovi.Kind() {
			case reflect.Map:
				log.Debugf("Merging: %v with %v", ovi.Interface(), nvi.Interface())
				m.mergeMaps(ovi, nvi)
			case reflect.Slice:
				log.Debugf("Merging: %v with %v", ovi.Interface(), nvi.Interface())
				ov.SetMapIndex(key, m.mergeArrays(ovi, nvi))
			case reflect.Array:
				log.Debugf("Merging: %v with %v", ovi.Interface(), nvi.Interface())
				ov.SetMapIndex(key, m.mergeArrays(ovi, nvi))
			}
		}
	}
}

func (m *merger) mergeArrays(ov, nv reflect.Value) reflect.Value {
Outer:
	for ni := 0; ni < nv.Len(); ni++ {
		niv := nv.Index(ni)
		for oi := 0; oi < ov.Len(); oi++ {
			oiv := ov.Index(oi)
			if reflect.DeepEqual(niv.Interface(), oiv.Interface()) {
				continue Outer
			}
		}
		log.Debugf("Appending %v to %v", niv.Interface(), ov)
		ov = reflect.Append(ov, niv)
	}
	return ov
}