// Copyright 2017 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:generate ./COMPILE-PROTOS.sh

// Gnostic is a tool for building better REST APIs through knowledge.
//
// Gnostic reads declarative descriptions of REST APIs that conform
// to the OpenAPI Specification, reports errors, resolves internal
// dependencies, and puts the results in a binary form that can
// be used in any language that is supported by the Protocol Buffer
// tools.
//
// Gnostic models are validated and typed. This allows API tool
// developers to focus on their product and not worry about input
// validation and type checking.
//
// Gnostic calls plugins that implement a variety of API implementation
// and support features including generation of client and server
// support code.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/golang/protobuf/proto"
	"github.com/googleapis/gnostic/OpenAPIv2"
	"github.com/googleapis/gnostic/OpenAPIv3"
	"github.com/googleapis/gnostic/compiler"
	plugins "github.com/googleapis/gnostic/plugins"
)

const ( // OpenAPI Version
	OpenAPIvUnknown = 0
	OpenAPIv2       = 2
	OpenAPIv3       = 3
)

// Determine the version of an OpenAPI description read from JSON or YAML.
func getOpenAPIVersionFromInfo(info interface{}) int {
	m, ok := compiler.UnpackMap(info)
	if !ok {
		return OpenAPIvUnknown
	}
	swagger, ok := compiler.MapValueForKey(m, "swagger").(string)
	if ok && swagger == "2.0" {
		return OpenAPIv2
	}
	openapi, ok := compiler.MapValueForKey(m, "openapi").(string)
	if ok && openapi == "3.0" {
		return OpenAPIv3
	}
	return OpenAPIvUnknown
}

const (
	pluginPrefix    = "gnostic-"
	extensionPrefix = "gnostic-x-"
)

type PluginCall struct {
	Name       string
	Invocation string
}

// Invokes a plugin.
func (pluginCall *PluginCall) perform(document proto.Message, openAPIVersion int, sourceName string) error {
	if pluginCall.Name != "" {
		request := &plugins.Request{}

		// Infer the name of the executable by adding the prefix.
		executableName := pluginPrefix + pluginCall.Name

		// validate invocation string with regular expression
		invocation := pluginCall.Invocation

		//
		// Plugin invocations must consist of
		// zero or more comma-separated key=value pairs followed by a path.
		// If pairs are present, a colon separates them from the path.
		// Keys and values must be alphanumeric strings and may contain
		// dashes, underscores, periods, or forward slashes.
		// A path can contain any characters other than the separators ',', ':', and '='.
		//
		invocation_regex := regexp.MustCompile(`^([\w-_\/\.]+=[\w-_\/\.]+(,[\w-_\/\.]+=[\w-_\/\.]+)*:)?[^,:=]+$`)
		if !invocation_regex.Match([]byte(pluginCall.Invocation)) {
			return errors.New(fmt.Sprintf("Invalid invocation of %s: %s", executableName, invocation))
		}

		invocationParts := strings.Split(pluginCall.Invocation, ":")
		var outputLocation string
		switch len(invocationParts) {
		case 1:
			outputLocation = invocationParts[0]
		case 2:
			parameters := strings.Split(invocationParts[0], ",")
			for _, keyvalue := range parameters {
				pair := strings.Split(keyvalue, "=")
				if len(pair) == 2 {
					request.Parameters = append(request.Parameters, &plugins.Parameter{Name: pair[0], Value: pair[1]})
				}
			}
			outputLocation = invocationParts[1]
		default:
			// badly-formed request
			outputLocation = invocationParts[len(invocationParts)-1]
		}

		version := &plugins.Version{}
		version.Major = 0
		version.Minor = 1
		version.Patch = 0
		request.CompilerVersion = version

		request.OutputPath = outputLocation

		wrapper := &plugins.Wrapper{}
		wrapper.Name = sourceName
		switch openAPIVersion {
		case OpenAPIv2:
			wrapper.Version = "v2"
		case OpenAPIv3:
			wrapper.Version = "v3"
		default:
			wrapper.Version = "unknown"
		}
		protoBytes, _ := proto.Marshal(document)
		wrapper.Value = protoBytes
		request.Wrapper = wrapper
		requestBytes, _ := proto.Marshal(request)

		cmd := exec.Command(executableName)
		cmd.Stdin = bytes.NewReader(requestBytes)
		cmd.Stderr = os.Stderr
		output, err := cmd.Output()
		if err != nil {
			return err
		}
		response := &plugins.Response{}
		err = proto.Unmarshal(output, response)
		if err != nil {
			return err
		}

		if response.Errors != nil {
			return errors.New(fmt.Sprintf("Plugin error: %+v", response.Errors))
		}

		// write files to the specified directory
		var writer io.Writer
		if outputLocation == "!" {
			// write nothing
		} else if outputLocation == "-" {
			writer = os.Stdout
			for _, file := range response.Files {
				writer.Write([]byte("\n\n" + file.Name + " -------------------- \n"))
				writer.Write(file.Data)
			}
		} else if isFile(outputLocation) {
			return errors.New(fmt.Sprintf("Error, unable to overwrite %s\n", outputLocation))
		} else {
			if !isDirectory(outputLocation) {
				os.Mkdir(outputLocation, 0755)
			}
			for _, file := range response.Files {
				p := outputLocation + "/" + file.Name
				dir := path.Dir(p)
				os.MkdirAll(dir, 0755)
				f, _ := os.Create(p)
				defer f.Close()
				f.Write(file.Data)
			}
		}
	}
	return nil
}

func isFile(path string) bool {
	fileInfo, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !fileInfo.IsDir()
}

func isDirectory(path string) bool {
	fileInfo, err := os.Stat(path)
	if err != nil {
		return false
	}
	return fileInfo.IsDir()
}

// Write bytes to a named file.
// Certain names have special meaning:
//   ! writes nothing
//   - writes to stdout
//   = writes to stderr
// If a directory name is given, the file is written there with
// a name derived from the source and extension arguments.
func writeFile(name string, bytes []byte, source string, extension string) {
	var writer io.Writer
	if name == "!" {
		return
	} else if name == "-" {
		writer = os.Stdout
	} else if name == "=" {
		writer = os.Stderr
	} else if isDirectory(name) {
		base := filepath.Base(source)
		// remove the original source extension
		base = base[0 : len(base)-len(filepath.Ext(base))]
		// build the path that puts the result in the passed-in directory
		filename := name + "/" + base + "." + extension
		file, _ := os.Create(filename)
		defer file.Close()
		writer = file
	} else {
		file, _ := os.Create(name)
		defer file.Close()
		writer = file
	}
	writer.Write(bytes)
	if name == "-" || name == "=" {
		writer.Write([]byte("\n"))
	}
}

// The Gnostic structure holds global state information for gnostic.
type Gnostic struct {
	usage             string
	sourceName        string
	binaryProtoPath   string
	jsonProtoPath     string
	textProtoPath     string
	errorPath         string
	resolveReferences bool
	pluginCalls       []*PluginCall
	extensionHandlers []compiler.ExtensionHandler
	openAPIVersion    int
}

// Initialize a structure to store global application state.
func newGnostic() *Gnostic {
	g := &Gnostic{}

	g.usage = `
Usage: gnostic OPENAPI_SOURCE [OPTIONS]
  OPENAPI_SOURCE is the filename or URL of an OpenAPI description to read.
Options:
  --pb-out=PATH       Write a binary proto to the specified location.
  --json-out=PATH     Write a json proto to the specified location.
  --text-out=PATH     Write a text proto to the specified location.
  --errors-out=PATH   Write compilation errors to the specified location.
  --PLUGIN-out=PATH   Run the plugin named gnostic_PLUGIN and write results
                      to the specified location.
  --x-EXTENSION       Use the extension named gnostic-x-EXTENSION
                      to process OpenAPI specification extensions.
  --resolve-refs      Explicitly resolve $ref references.
                      This could have problems with recursive definitions.
`
	// default values for all options
	g.sourceName = ""
	g.binaryProtoPath = ""
	g.jsonProtoPath = ""
	g.textProtoPath = ""
	g.errorPath = ""
	g.resolveReferences = false

	// internal structures
	g.pluginCalls = make([]*PluginCall, 0)
	g.extensionHandlers = make([]compiler.ExtensionHandler, 0)
	return g
}

// Parse command-line options.
func (g *Gnostic) readOptions() {
	// plugin processing matches patterns of the form "--PLUGIN-out=PATH" and "--PLUGIN_out=PATH"
	plugin_regex := regexp.MustCompile("--(.+)[-_]out=(.+)")

	// extension processing matches patterns of the form "--x-EXTENSION"
	extension_regex := regexp.MustCompile("--x-(.+)")

	for i, arg := range os.Args {
		if i == 0 {
			continue // skip the tool name
		}
		var m [][]byte
		if m = plugin_regex.FindSubmatch([]byte(arg)); m != nil {
			pluginName := string(m[1])
			invocation := string(m[2])
			switch pluginName {
			case "pb":
				g.binaryProtoPath = invocation
			case "json":
				g.jsonProtoPath = invocation
			case "text":
				g.textProtoPath = invocation
			case "errors":
				g.errorPath = invocation
			default:
				pluginCall := &PluginCall{Name: pluginName, Invocation: invocation}
				g.pluginCalls = append(g.pluginCalls, pluginCall)
			}
		} else if m = extension_regex.FindSubmatch([]byte(arg)); m != nil {
			extensionName := string(m[1])
			extensionHandler := compiler.ExtensionHandler{Name: extensionPrefix + extensionName}
			g.extensionHandlers = append(g.extensionHandlers, extensionHandler)
		} else if arg == "--resolve-refs" {
			g.resolveReferences = true
		} else if arg[0] == '-' {
			fmt.Fprintf(os.Stderr, "Unknown option: %s.\n%s\n", arg, g.usage)
			os.Exit(-1)
		} else {
			g.sourceName = arg
		}
	}
}

// Validate command-line options
func (g *Gnostic) validateOptions() {
	if g.binaryProtoPath == "" &&
		g.jsonProtoPath == "" &&
		g.textProtoPath == "" &&
		g.errorPath == "" &&
		len(g.pluginCalls) == 0 {
		fmt.Fprintf(os.Stderr, "Missing output directives.\n%s\n", g.usage)
		os.Exit(-1)
	}
	if g.sourceName == "" {
		fmt.Fprintf(os.Stderr, "No input specified.\n%s\n", g.usage)
		os.Exit(-1)
	}
	// If we get here and the error output is unspecified, write errors to stderr.
	if g.errorPath == "" {
		g.errorPath = "="
	}
}

// Generate an error message to be written to stderr or a file.
func (g *Gnostic) errorBytes(err error) []byte {
	return []byte("Errors reading " + g.sourceName + "\n" + err.Error())
}

// Read an OpenAPI description from YAML or JSON.
func (g *Gnostic) readOpenAPIText(bytes []byte) (message proto.Message, err error) {
	info, err := compiler.ReadInfoFromBytes(g.sourceName, bytes)
	if err != nil {
		return nil, err
	}
	// Determine the OpenAPI version.
	g.openAPIVersion = getOpenAPIVersionFromInfo(info)
	if g.openAPIVersion == OpenAPIvUnknown {
		return nil, errors.New("Unable to identify OpenAPI version.")
	}
	// Compile to the proto model.
	if g.openAPIVersion == OpenAPIv2 {
		document, err := openapi_v2.NewDocument(info, compiler.NewContextWithExtensions("$root", nil, &g.extensionHandlers))
		if err != nil {
			return nil, err
		}
		message = document
	} else if g.openAPIVersion == OpenAPIv3 {
		document, err := openapi_v3.NewDocument(info, compiler.NewContextWithExtensions("$root", nil, &g.extensionHandlers))
		if err != nil {
			return nil, err
		}
		message = document
	}
	return message, err
}

// Read an OpenAPI binary file.
func (g *Gnostic) readOpenAPIBinary(data []byte) (message proto.Message, err error) {
	// try to read an OpenAPI v3 document
	document_v3 := &openapi_v3.Document{}
	err = proto.Unmarshal(data, document_v3)
	if err == nil {
		return document_v3, nil
	}
	// if that failed, try to read an OpenAPI v2 document
	document_v2 := &openapi_v2.Document{}
	err = proto.Unmarshal(data, document_v2)
	if err == nil {
		return document_v2, nil
	}
	return nil, err
}

// Perform all actions specified in the command-line options.
func (g *Gnostic) performActions(message proto.Message) (err error) {
	// Optionally resolve internal references.
	if g.resolveReferences {
		if g.openAPIVersion == OpenAPIv2 {
			document := message.(*openapi_v2.Document)
			_, err = document.ResolveReferences(g.sourceName)
		} else if g.openAPIVersion == OpenAPIv3 {
			document := message.(*openapi_v3.Document)
			_, err = document.ResolveReferences(g.sourceName)
		}
		if err != nil {
			return err
		}
	}
	// Optionally write proto in binary format.
	if g.binaryProtoPath != "" {
		protoBytes, err := proto.Marshal(message)
		if err != nil {
			writeFile(g.errorPath, g.errorBytes(err), g.sourceName, "errors")
			defer os.Exit(-1)
		} else {
			writeFile(g.binaryProtoPath, protoBytes, g.sourceName, "pb")
		}
	}
	// Optionally write proto in json format.
	if g.jsonProtoPath != "" {
		jsonBytes, err := json.Marshal(message)
		if err != nil {
			writeFile(g.errorPath, g.errorBytes(err), g.sourceName, "errors")
			defer os.Exit(-1)
		} else {
			writeFile(g.jsonProtoPath, jsonBytes, g.sourceName, "json")
		}
	}
	// Optionally write proto in text format.
	if g.textProtoPath != "" {
		bytes := []byte(proto.MarshalTextString(message))
		writeFile(g.textProtoPath, bytes, g.sourceName, "text")
	}
	// Call all specified plugins.
	for _, pluginCall := range g.pluginCalls {
		err := pluginCall.perform(message, g.openAPIVersion, g.sourceName)
		if err != nil {
			writeFile(g.errorPath, g.errorBytes(err), g.sourceName, "errors")
			defer os.Exit(-1) // run all plugins, even when some have errors
		}
	}
	return nil
}

func (g *Gnostic) main() {
	var err error
	g.readOptions()
	g.validateOptions()

	// Read the OpenAPI source.
	bytes, err := compiler.ReadBytesForFile(g.sourceName)
	if err != nil {
		writeFile(g.errorPath, g.errorBytes(err), g.sourceName, "errors")
		os.Exit(-1)
	}
	extension := strings.ToLower(filepath.Ext(g.sourceName))
	var message proto.Message
	if extension == ".json" || extension == ".yaml" {
		// Try to read the source as JSON/YAML.
		message, err = g.readOpenAPIText(bytes)
		if err != nil {
			writeFile(g.errorPath, g.errorBytes(err), g.sourceName, "errors")
			os.Exit(-1)
		}
	} else if extension == ".pb" {
		// Try to read the source as a binary protocol buffer.
		message, err = g.readOpenAPIBinary(bytes)
		if err != nil {
			writeFile(g.errorPath, g.errorBytes(err), g.sourceName, "errors")
			os.Exit(-1)
		}
	} else {
		err = errors.New("Unknown file extension. 'json', 'yaml', and 'pb' are accepted.")
		writeFile(g.errorPath, g.errorBytes(err), g.sourceName, "errors")
		os.Exit(-1)
	}
	err = g.performActions(message)
	if err != nil {
		writeFile(g.errorPath, g.errorBytes(err), g.sourceName, "errors")
		os.Exit(-1)
	}
}

func main() {
	g := newGnostic()
	g.main()
}
