package main

import (
	"fmt"
	"reflect"
	"strings"
)

var httpMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

func isMethod(k string) bool {
	for _, m := range httpMethods {
		if m == k {
			return true
		}
	}
	return false
}

// diffOpenAPI compares two merged OpenAPI 3.1 documents (see mergeOpenAPI).
func diffOpenAPI(o, n object) []Change {
	var out []Change
	d := differ{subject: "openapi", out: &out}
	d.diffPaths("/paths", asObject(o["paths"]), asObject(n["paths"]), o, n)
	for _, k := range unionKeys(o, n) {
		ov, oHas := o[k]
		nv, nHas := n[k]
		if oHas && nHas && reflect.DeepEqual(ov, nv) {
			continue
		}
		switch k {
		case "paths":
			// compared above, because a root-level security change alters operations that did not change
		case "components":
			d.diffComponents("/components", asObject(ov), asObject(nv))
		case "security":
			// the effective security of each operation is compared with the operation
		default:
			d.diffAnnotation("/"+esc(k), k, ov, oHas, nv, nHas)
		}
	}
	return out
}

// diffAnnotation: info, servers, tags, x-* and other descriptive members never change a call; x-alixai-* ones do.
func (d differ) diffAnnotation(ptr, key string, ov any, oHas bool, nv any, nHas bool) {
	switch {
	case strings.HasPrefix(key, "x-alixai-"):
		switch {
		case !oHas:
			d.add(Additive, ptr, "extension_added", "extension %q added", key)
		default:
			d.add(Breaking, ptr, "extension_changed", "extension %q changed or removed", key)
		}
	default:
		d.add(DocsOnly, ptr, "docs_changed", "%q changed", key)
	}
}

func effectiveSecurity(root, op object) any {
	if s, ok := op["security"]; ok {
		return s
	}
	return root["security"]
}

func (d differ) diffPaths(ptr string, o, n, oroot, nroot object) {
	for _, p := range unionKeys(o, n) {
		pp := ptr + "/" + esc(p)
		ov, oHas := o[p]
		nv, nHas := n[p]
		switch {
		case oHas && !nHas:
			d.add(Breaking, pp, "path_removed", "path %s was removed", p)
		case !oHas && nHas:
			d.add(Additive, pp, "path_added", "path %s was added", p)
		default:
			d.diffPathItem(pp, p, asObject(ov), asObject(nv), oroot, nroot)
		}
	}
}

func (d differ) diffPathItem(ptr, path string, o, n, oroot, nroot object) {
	for _, k := range unionKeys(o, n) {
		ov, oHas := o[k]
		nv, nHas := n[k]
		if oHas && nHas && reflect.DeepEqual(ov, nv) {
			// the operation itself may still differ through the root security
			if isMethod(k) && !reflect.DeepEqual(effectiveSecurity(oroot, asObject(ov)), effectiveSecurity(nroot, asObject(nv))) {
				d.add(Breaking, ptr+"/"+k, "security_changed", "%s %s: the security requirement changed", strings.ToUpper(k), path)
			}
			continue
		}
		kp := ptr + "/" + esc(k)
		switch {
		case isMethod(k):
			switch {
			case !nHas:
				d.add(Breaking, kp, "operation_removed", "operation %s %s was removed", strings.ToUpper(k), path)
			case !oHas:
				d.add(Additive, kp, "operation_added", "operation %s %s was added", strings.ToUpper(k), path)
			default:
				d.diffOperation(kp, strings.ToUpper(k)+" "+path, asObject(ov), asObject(nv), oroot, nroot)
			}
		case k == "parameters":
			d.diffParameters(kp, asList(ov), asList(nv))
		default:
			d.diffAnnotation(kp, k, ov, oHas, nv, nHas)
		}
	}
}

func (d differ) diffOperation(ptr, label string, o, n, oroot, nroot object) {
	if !reflect.DeepEqual(effectiveSecurity(oroot, o), effectiveSecurity(nroot, n)) {
		d.add(Breaking, ptr+"/security", "security_changed", "%s: the security requirement changed", label)
	}
	for _, k := range unionKeys(o, n) {
		ov, oHas := o[k]
		nv, nHas := n[k]
		if oHas && nHas && reflect.DeepEqual(ov, nv) {
			continue
		}
		kp := ptr + "/" + esc(k)
		switch k {
		case "security":
			// handled above
		case "operationId":
			d.add(Breaking, kp, "operation_id_changed", "%s: operationId changed (%s -> %s)", label, show(ov, oHas), show(nv, nHas))
		case "parameters":
			d.diffParameters(kp, asList(ov), asList(nv))
		case "requestBody":
			d.diffRequestBody(kp, label, ov, oHas, nv, nHas)
		case "responses":
			d.diffResponses(kp, asObject(ov), asObject(nv))
		case "tags", "summary", "description", "deprecated", "externalDocs":
			d.add(DocsOnly, kp, "docs_changed", "%s: %q changed", label, k)
		case "servers", "callbacks":
			d.add(Breaking, kp, "operation_changed", "%s: %q changed", label, k)
		default:
			d.diffAnnotation(kp, k, ov, oHas, nv, nHas)
		}
	}
}

func paramKey(p any) string {
	m := asObject(p)
	if r, ok := m["$ref"].(string); ok {
		return "ref:" + r
	}
	return fmt.Sprint(m["in"]) + ":" + fmt.Sprint(m["name"])
}

func (d differ) diffParameters(ptr string, o, n []any) {
	om, nm := map[string]any{}, map[string]any{}
	var keys []string
	seen := map[string]bool{}
	for _, p := range o {
		om[paramKey(p)] = p
		if !seen[paramKey(p)] {
			seen[paramKey(p)] = true
			keys = append(keys, paramKey(p))
		}
	}
	for _, p := range n {
		nm[paramKey(p)] = p
		if !seen[paramKey(p)] {
			seen[paramKey(p)] = true
			keys = append(keys, paramKey(p))
		}
	}
	for _, k := range keys {
		ov, oHas := om[k]
		nv, nHas := nm[k]
		kp := ptr + "/" + esc(k)
		switch {
		case oHas && !nHas:
			d.add(Breaking, kp, "parameter_removed", "parameter %s was removed", k)
		case !oHas && nHas:
			if truthy(asObject(nv)["required"]) || asObject(nv)["in"] == "path" {
				d.add(Breaking, kp, "new_required_parameter", "required parameter %s was added", k)
			} else if _, isRef := asObject(nv)["$ref"]; isRef {
				d.add(Additive, kp, "new_parameter_ref", "parameter %s was added by reference (its component is compared separately)", k)
			} else {
				d.add(Additive, kp, "new_optional_parameter", "optional parameter %s was added", k)
			}
		default:
			d.diffParameter(kp, asObject(ov), asObject(nv))
		}
	}
}

func (d differ) diffParameter(ptr string, o, n object) {
	for _, k := range unionKeys(o, n) {
		ov, oHas := o[k]
		nv, nHas := n[k]
		if oHas && nHas && reflect.DeepEqual(ov, nv) {
			continue
		}
		kp := ptr + "/" + esc(k)
		switch k {
		case "required":
			if truthy(nv) && !truthy(ov) {
				d.add(Breaking, kp, "optional_became_required", "parameter became required")
			} else if truthy(ov) && !truthy(nv) {
				d.add(Additive, kp, "required_relaxed", "parameter is no longer required")
			}
		case "schema":
			d.diffSchema(kp, schemaOrTrue(ov, oHas), schemaOrTrue(nv, nHas))
		case "$ref":
			d.add(Breaking, kp, "ref_changed", "$ref changed (%s -> %s)", show(ov, oHas), show(nv, nHas))
		case "in", "name", "style", "explode", "allowReserved", "content", "allowEmptyValue":
			d.add(Breaking, kp, "parameter_changed", "parameter member %q changed (%s -> %s)", k, show(ov, oHas), show(nv, nHas))
		default:
			d.diffAnnotation(kp, k, ov, oHas, nv, nHas)
		}
	}
}

func (d differ) diffRequestBody(ptr, label string, ov any, oHas bool, nv any, nHas bool) {
	switch {
	case oHas && !nHas:
		d.add(Breaking, ptr, "request_body_removed", "%s: the request body was removed", label)
		return
	case !oHas && nHas:
		if truthy(asObject(nv)["required"]) {
			d.add(Breaking, ptr, "new_required_request_body", "%s: a required request body was added", label)
		} else {
			d.add(Additive, ptr, "request_body_added", "%s: an optional request body was added", label)
		}
		return
	}
	d.diffBody(ptr, asObject(ov), asObject(nv))
}

func (d differ) diffBody(ptr string, o, n object) {
	for _, k := range unionKeys(o, n) {
		ov, oHas := o[k]
		nv, nHas := n[k]
		if oHas && nHas && reflect.DeepEqual(ov, nv) {
			continue
		}
		kp := ptr + "/" + esc(k)
		switch k {
		case "required":
			if truthy(nv) && !truthy(ov) {
				d.add(Breaking, kp, "optional_became_required", "the request body became required")
			} else if truthy(ov) && !truthy(nv) {
				d.add(Additive, kp, "required_relaxed", "the request body is no longer required")
			}
		case "content":
			d.diffContent(kp, asObject(ov), asObject(nv))
		case "$ref":
			d.add(Breaking, kp, "ref_changed", "$ref changed (%s -> %s)", show(ov, oHas), show(nv, nHas))
		default:
			d.diffAnnotation(kp, k, ov, oHas, nv, nHas)
		}
	}
}

func (d differ) diffContent(ptr string, o, n object) {
	for _, mt := range unionKeys(o, n) {
		kp := ptr + "/" + esc(mt)
		ov, oHas := o[mt]
		nv, nHas := n[mt]
		switch {
		case oHas && !nHas:
			d.add(Breaking, kp, "media_type_removed", "media type %s was removed", mt)
		case !oHas && nHas:
			d.add(Additive, kp, "media_type_added", "media type %s was added", mt)
		default:
			om, nm := asObject(ov), asObject(nv)
			for _, k := range unionKeys(om, nm) {
				a, aHas := om[k]
				b, bHas := nm[k]
				if aHas && bHas && reflect.DeepEqual(a, b) {
					continue
				}
				switch k {
				case "schema":
					d.diffSchema(kp+"/schema", schemaOrTrue(a, aHas), schemaOrTrue(b, bHas))
				case "encoding", "itemSchema":
					d.add(Breaking, kp+"/"+k, "media_type_changed", "%q changed for %s", k, mt)
				default:
					d.diffAnnotation(kp+"/"+esc(k), k, a, aHas, b, bHas)
				}
			}
		}
	}
}

func (d differ) diffResponses(ptr string, o, n object) {
	for _, code := range unionKeys(o, n) {
		kp := ptr + "/" + esc(code)
		ov, oHas := o[code]
		nv, nHas := n[code]
		switch {
		case oHas && !nHas:
			d.add(Breaking, kp, "response_removed", "response %s was removed", code)
		case !oHas && nHas:
			d.add(Additive, kp, "response_added", "response %s was added", code)
		default:
			d.diffResponse(kp, asObject(ov), asObject(nv))
		}
	}
}

func (d differ) diffResponse(ptr string, o, n object) {
	for _, k := range unionKeys(o, n) {
		ov, oHas := o[k]
		nv, nHas := n[k]
		if oHas && nHas && reflect.DeepEqual(ov, nv) {
			continue
		}
		kp := ptr + "/" + esc(k)
		switch k {
		case "$ref":
			d.add(Breaking, kp, "ref_changed", "$ref changed (%s -> %s)", show(ov, oHas), show(nv, nHas))
		case "content":
			d.diffContent(kp, asObject(ov), asObject(nv))
		case "headers":
			d.diffHeaders(kp, asObject(ov), asObject(nv))
		default:
			d.diffAnnotation(kp, k, ov, oHas, nv, nHas)
		}
	}
}

func (d differ) diffHeaders(ptr string, o, n object) {
	for _, h := range unionKeys(o, n) {
		kp := ptr + "/" + esc(h)
		ov, oHas := o[h]
		nv, nHas := n[h]
		switch {
		case oHas && !nHas:
			d.add(Breaking, kp, "header_removed", "response header %s was removed", h)
		case !oHas && nHas:
			d.add(Additive, kp, "header_added", "response header %s was added", h)
		default:
			om, nm := asObject(ov), asObject(nv)
			for _, k := range unionKeys(om, nm) {
				a, aHas := om[k]
				b, bHas := nm[k]
				if aHas && bHas && reflect.DeepEqual(a, b) {
					continue
				}
				switch k {
				case "schema":
					d.diffSchema(kp+"/schema", schemaOrTrue(a, aHas), schemaOrTrue(b, bHas))
				case "required":
					if truthy(a) && !truthy(b) {
						d.add(Breaking, kp+"/required", "required_removed", "response header %s is no longer guaranteed", h)
					}
				default:
					d.diffAnnotation(kp+"/"+esc(k), k, a, aHas, b, bHas)
				}
			}
		}
	}
}

func (d differ) diffComponents(ptr string, o, n object) {
	for _, section := range unionKeys(o, n) {
		sp := ptr + "/" + esc(section)
		ov, oHas := o[section]
		nv, nHas := n[section]
		if oHas && nHas && reflect.DeepEqual(ov, nv) {
			continue
		}
		om, nm := asObject(ov), asObject(nv)
		for _, name := range unionKeys(om, nm) {
			cp := sp + "/" + esc(name)
			a, aHas := om[name]
			b, bHas := nm[name]
			switch {
			case aHas && !bHas:
				d.add(Breaking, cp, "component_removed", "component %s/%s was removed", section, name)
			case !aHas && bHas:
				d.add(Additive, cp, "component_added", "component %s/%s was added", section, name)
			case reflect.DeepEqual(a, b):
			default:
				switch section {
				case "schemas":
					d.diffSchema(cp, a, b)
				case "parameters":
					d.diffParameter(cp, asObject(a), asObject(b))
				case "responses":
					d.diffResponse(cp, asObject(a), asObject(b))
				case "requestBodies":
					d.diffBody(cp, asObject(a), asObject(b))
				case "headers":
					d.diffHeaders(sp, object{name: a}, object{name: b})
				case "securitySchemes":
					d.add(Breaking, cp, "security_scheme_changed", "security scheme %s changed", name)
				default: // examples, links, callbacks, pathItems
					d.add(DocsOnly, cp, "docs_changed", "component %s/%s changed", section, name)
				}
			}
		}
	}
}
