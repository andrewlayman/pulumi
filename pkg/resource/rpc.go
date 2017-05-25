// Licensed to Pulumi Corporation ("Pulumi") under one or more
// contributor license agreements.  See the NOTICE file distributed with
// this work for additional information regarding copyright ownership.
// Pulumi licenses this file to You under the Apache License, Version 2.0
// (the "License"); you may not use this file except in compliance with
// the License.  You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package resource

import (
	"reflect"
	"sort"

	"github.com/golang/glog"
	structpb "github.com/golang/protobuf/ptypes/struct"

	"github.com/pulumi/lumi/pkg/util/contract"
)

// MarshalOptions controls the marshaling of RPC structures.
type MarshalOptions struct {
	PermitOlds bool // true to permit old URNs in the properties (e.g., for pre-update).
	RawURNs    bool // true to marshal URNs "as-is"; often used when ID mappings aren't known yet.
}

// MarshalPropertiesWithUnknowns marshals a resource's property map as a "JSON-like" protobuf structure.  Any URNs are
// replaced with their resource IDs during marshaling; it is an error to marshal a URN for a resource without an ID.  A
// map of any unknown properties encountered during marshaling (latent values) is returned on the side; these values are
// marshaled using the default value in the returned structure and so this map is essential for interpreting results.
func MarshalPropertiesWithUnknowns(
	ctx *Context, props PropertyMap, opts MarshalOptions) (*structpb.Struct, map[string]bool) {
	var unk map[string]bool
	result := &structpb.Struct{
		Fields: make(map[string]*structpb.Value),
	}
	for _, key := range StablePropertyKeys(props) {
		v := props[key]
		if !v.IsOutput() { // always skip output properties.
			mv, known := MarshalPropertyValue(ctx, props[key], opts)
			result.Fields[string(key)] = mv
			if !known {
				if unk == nil {
					unk = make(map[string]bool)
				}
				unk[string(key)] = true // remember that this property was unknown, tainting this whole object.
			}
		}
	}
	return result, unk
}

// MarshalProperties performs ordinary marshaling of a resource's properties but then validates afterwards that all
// fields were known (in other words, no latent properties were encountered).
func MarshalProperties(ctx *Context, props PropertyMap, opts MarshalOptions) *structpb.Struct {
	pstr, unks := MarshalPropertiesWithUnknowns(ctx, props, opts)
	contract.Assertf(unks == nil, "Unexpected unknown properties during final marshaling")
	return pstr
}

// MarshalPropertyValue marshals a single resource property value into its "JSON-like" value representation.  The
// boolean return value indicates whether the value was known (true) or unknown (false).
func MarshalPropertyValue(ctx *Context, v PropertyValue, opts MarshalOptions) (*structpb.Value, bool) {
	if v.IsNull() {
		return &structpb.Value{
			Kind: &structpb.Value_NullValue{
				NullValue: structpb.NullValue_NULL_VALUE,
			},
		}, true
	} else if v.IsBool() {
		return &structpb.Value{
			Kind: &structpb.Value_BoolValue{
				BoolValue: v.BoolValue(),
			},
		}, true
	} else if v.IsNumber() {
		return &structpb.Value{
			Kind: &structpb.Value_NumberValue{
				NumberValue: v.NumberValue(),
			},
		}, true
	} else if v.IsString() {
		return &structpb.Value{
			Kind: &structpb.Value_StringValue{
				StringValue: v.StringValue(),
			},
		}, true
	} else if v.IsArray() {
		outcome := true
		var elems []*structpb.Value
		for _, elem := range v.ArrayValue() {
			elemv, known := MarshalPropertyValue(ctx, elem, opts)
			outcome = outcome && known
			elems = append(elems, elemv)
		}
		return &structpb.Value{
			Kind: &structpb.Value_ListValue{
				ListValue: &structpb.ListValue{Values: elems},
			},
		}, outcome
	} else if v.IsObject() {
		obj, unks := MarshalPropertiesWithUnknowns(ctx, v.ObjectValue(), opts)
		return &structpb.Value{
			Kind: &structpb.Value_StructValue{
				StructValue: obj,
			},
		}, unks == nil
	} else if v.IsResource() {
		var wire string
		m := v.ResourceValue()
		if opts.RawURNs {
			wire = string(m)
		} else {
			contract.Assertf(ctx != nil, "Resource encountered with a nil context; URN not recoverable")
			var id ID
			if res, has := ctx.URNRes[m]; has {
				id = res.ID() // found a new resource with this ID, use it.
			} else if oldid, has := ctx.URNOldIDs[m]; opts.PermitOlds && has {
				id = oldid // found an old resource, maybe deleted, so use that.
			} else {
				contract.Failf("Expected resource URN '%v' to exist at marshal time", m)
			}
			contract.Assertf(id != "", "Expected resource URN '%v' to have an ID at marshal time", m)
			wire = string(id)
		}
		glog.V(7).Infof("Serializing resource URN '%v' as '%v' (raw=%v)", m, wire, opts.RawURNs)
		return &structpb.Value{
			Kind: &structpb.Value_StringValue{
				StringValue: wire,
			},
		}, true
	} else if v.IsComputed() {
		v, _ := MarshalPropertyValue(ctx, v.ComputedValue().Eventual(), opts)
		return v, false
	} else if v.IsOutput() {
		v, _ := MarshalPropertyValue(ctx, v.OutputValue().Eventual(), opts)
		return v, false
	}

	contract.Failf("Unrecognized property value: %v (type=%v)", v.V, reflect.TypeOf(v.V))
	return nil, true
}

// UnmarshalProperties unmarshals a "JSON-like" protobuf structure into a resource property map.
func UnmarshalProperties(props *structpb.Struct) PropertyMap {
	result := make(PropertyMap)
	if props == nil {
		return result
	}

	// First sort the keys so we enumerate them in order (in case errors happen, we want determinism).
	var keys []string
	for k := range props.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// And now unmarshal every field it into the map.
	for _, k := range keys {
		result[PropertyKey(k)] = UnmarshalPropertyValue(props.Fields[k])
	}

	return result
}

// UnmarshalPropertyValue unmarshals a single "JSON-like" value into its property form.
func UnmarshalPropertyValue(v *structpb.Value) PropertyValue {
	if v != nil {
		switch v.Kind.(type) {
		case *structpb.Value_NullValue:
			return NewNullProperty()
		case *structpb.Value_BoolValue:
			return NewBoolProperty(v.GetBoolValue())
		case *structpb.Value_NumberValue:
			return NewNumberProperty(v.GetNumberValue())
		case *structpb.Value_StringValue:
			// TODO: we have no way of determining that this is a resource ID; consider tagging.
			return NewStringProperty(v.GetStringValue())
		case *structpb.Value_ListValue:
			var elems []PropertyValue
			lst := v.GetListValue()
			for _, elem := range lst.GetValues() {
				elems = append(elems, UnmarshalPropertyValue(elem))
			}
			return NewArrayProperty(elems)
		case *structpb.Value_StructValue:
			props := UnmarshalProperties(v.GetStructValue())
			return NewObjectProperty(props)
		default:
			contract.Failf("Unrecognized structpb value kind: %v", reflect.TypeOf(v.Kind))
		}
	}
	return NewNullProperty()
}
