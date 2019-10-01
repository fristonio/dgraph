/*
 * Copyright 2019 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package schema

import (
	"fmt"
	"strings"

	"github.com/vektah/gqlparser/ast"
	"github.com/vektah/gqlparser/gqlerror"
	"github.com/vektah/gqlparser/parser"
	"github.com/vektah/gqlparser/validator"
)

// A Handler can produce valid GraphQL and Dgraph schemas given an input of
// types and relationships
type Handler interface {
	DGSchema() string
	GQLSchema() string
}

type handler struct {
	input          string
	originalDefs   []string
	completeSchema *ast.Schema
	dgraphSchema   string
}

func (s *handler) GQLSchema() string {
	return Stringify(s.completeSchema, s.originalDefs)
}

func (s *handler) DGSchema() string {
	return s.dgraphSchema
}

// NewHandler processes the input schema.  If there are no errors, it returns
// a valid Handler, otherwise it returns nil and an error.
func NewHandler(input string) (Handler, error) {
	if input == "" {
		return nil, gqlerror.Errorf("No schema specified")
	}

	// The input schema contains just what's required to describe the types,
	// relationships and searchability - but that's not enough to define a
	// valid GraphQL schema: e.g. we allow an input schema file like
	//
	// type T {
	//   f: Int @searchable
	// }
	//
	// But, that's not valid GraphQL unless there's also definitions of scalars
	// (Int, String, etc) and definitions of the directives (@searchable, etc).
	// We don't want to make the user have those in their file and then we have
	// to check that they've made the right definitions, etc, etc.
	//
	// So we parse the original input of just types and relationships and
	// run a validation to make sure it only contains things that it should.
	// To that we add all the scalars and other definitions we always require.
	//
	// Then, we GraphQL validate to make sure their definitions plus our additions
	// is GraphQL valid.  At this point we know the definitions are GraphQL valid,
	// but we need to check if it makes sense to our layer.
	//
	// The next final validation ensures that the definitions are made
	// in such a way that our GraphQL API will be able to interpret the schema
	// correctly.
	//
	// Then we can complete the process by adding in queries and mutations etc. to
	// make the final full GraphQL schema.

	doc, gqlErr := parser.ParseSchemas(validator.Prelude, &ast.Source{Input: input})
	if gqlErr != nil {
		return nil, gqlerror.List{gqlErr}
	}

	gqlErrList := preGQLValidation(doc)
	if gqlErrList != nil {
		return nil, gqlErrList
	}

	defns := make([]string, 0, len(doc.Definitions))
	for _, defn := range doc.Definitions {
		if defn.BuiltIn {
			continue
		}
		defns = append(defns, defn.Name)
	}

	expandSchema(doc)

	sch, gqlErr := validator.ValidateSchemaDocument(doc)
	if gqlErr != nil {
		return nil, gqlerror.List{gqlErr}
	}

	gqlErrList = postGQLValidation(sch, defns)
	if gqlErrList != nil {
		return nil, gqlErrList
	}

	dgSchema := genDgSchema(sch, defns)
	completeSchema(sch, defns)

	return &handler{
		input:          input,
		dgraphSchema:   dgSchema,
		completeSchema: sch,
		originalDefs:   defns,
	}, nil
}

// parentInterface returns the name of an interface that a field belonging to a type definition
// typDef inherited from. If there is no such interface, then it returns an empty string.
//
// Given the following schema
// interface A {
//   name: String
// }
//
// type B implements A {
//	 name: String
//   age: Int
// }
//
// calling parentInterface on the fieldName name with type definition for B, would return A.
func parentInterface(sch *ast.Schema, typDef *ast.Definition, fieldName string) string {
	if len(typDef.Interfaces) == 0 {
		return ""
	}

	for _, iface := range typDef.Interfaces {
		interfaceDef := sch.Types[iface]
		for _, interfaceField := range interfaceDef.Fields {
			if fieldName == interfaceField.Name {
				return iface
			}
		}
	}
	return ""
}

func DgraphMapping(sch *ast.Schema) map[string]string {
	const (
		del     = "Delete"
		payload = "Payload"
	)

	dgraphPredicate := make(map[string]string)
	for _, inputTyp := range sch.Types {
		if inputTyp.BuiltIn || inputTyp.Name == "query" || inputTyp.Name == "mutation" ||
			(inputTyp.Kind != ast.Object && inputTyp.Kind != ast.Interface) {
			continue
		}

		originalTyp := inputTyp
		inputTypeName := inputTyp.Name
		if strings.HasPrefix(inputTypeName, del) && strings.HasSuffix(inputTypeName, payload) {
			inputTypeName = strings.TrimSuffix(strings.TrimPrefix(inputTypeName, del), payload)
			inputTyp = sch.Types[inputTypeName]
		}
		// TODO - This also includes Update, Delete and Add type payload objects. Also interfaces.
		for _, fld := range inputTyp.Fields {
			typName := inputTypeName
			parentInt := parentInterface(sch, inputTyp, fld.Name)
			if parentInt != "" {
				typName = parentInt
			}
			dgraphPredicate[originalTyp.Name+fld.Name] = typName + "." + fld.Name
		}
	}
	return dgraphPredicate
}

// genDgSchema generates Dgraph schema from a valid graphql schema.
func genDgSchema(gqlSch *ast.Schema, definitions []string) string {
	var typeStrings []string

	for _, key := range definitions {
		def := gqlSch.Types[key]
		switch def.Kind {
		case ast.Object, ast.Interface:
			var typeDef, preds strings.Builder
			fmt.Fprintf(&typeDef, "type %s {\n", def.Name)
			for _, f := range def.Fields {
				if f.Type.Name() == "ID" {
					continue
				}

				typName := def.Name
				// This field could have originally been defined in an interface that this type
				// implements. If we get a parent interface, then we should prefix the field name
				// with it instead of def.Name.
				parentInt := parentInterface(gqlSch, def, f.Name)
				if parentInt != "" {
					typName = parentInt
				}

				var prefix, suffix string
				if f.Type.Elem != nil {
					prefix = "["
					suffix = "]"
				}

				var typStr string
				switch gqlSch.Types[f.Type.Name()].Kind {
				case ast.Object:
					typStr = fmt.Sprintf("%suid%s", prefix, suffix)

					fmt.Fprintf(&typeDef, "  %s.%s: %s\n", typName, f.Name, typStr)
					if parentInt == "" {
						fmt.Fprintf(&preds, "%s.%s: %s .\n", typName, f.Name, typStr)
					}
				case ast.Scalar:
					typStr = fmt.Sprintf(
						"%s%s%s",
						prefix, scalarToDgraph[f.Type.Name()], suffix,
					)

					indexStr := ""
					searchable := f.Directives.ForName(searchableDirective)
					if searchable != nil {
						arg := searchable.Arguments.ForName(searchableArg)
						if arg != nil {
							indexStr = fmt.Sprintf(" @index(%s)", arg.Value.Raw)
						} else {
							indexStr = fmt.Sprintf(" @index(%s)", defaultSearchables[f.Type.Name()])
						}
					}

					fmt.Fprintf(&typeDef, "  %s.%s: %s\n", typName, f.Name, typStr)
					if parentInt == "" {
						fmt.Fprintf(&preds, "%s.%s: %s%s .\n", typName, f.Name, typStr, indexStr)
					}
				case ast.Enum:
					fmt.Fprintf(&typeDef, "  %s.%s: string\n", typName, f.Name)
					if parentInt == "" {
						fmt.Fprintf(&preds, "%s.%s: string @index(exact) .\n", typName, f.Name)
					}
				}
			}
			fmt.Fprintf(&typeDef, "}\n")

			typeStrings = append(
				typeStrings,
				fmt.Sprintf("%s%s", typeDef.String(), preds.String()),
			)
		}
	}

	return strings.Join(typeStrings, "")
}