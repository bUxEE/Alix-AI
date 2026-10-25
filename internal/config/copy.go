package config

import "reflect"

// copyValue deep-copies src into dst, which must be settable and of the same
// type. Pointers, maps, slices, arrays and interface contents are duplicated
// so that dst shares no mutable memory with src. This matters because
// yaml.v3 decodes into an existing map by merging keys and through an existing
// pointer by writing to its target: with a shallow copy, a Load that later
// fails would already have written into the defaults and the live section.
//
// Structs are copied whole first (which also carries unexported fields along,
// shallowly) and their exported fields are then deep-copied. Configuration
// types are plain trees; a cyclic value would recurse forever.
func copyValue(dst, src reflect.Value) {
	switch src.Kind() {
	case reflect.Pointer:
		if src.IsNil() {
			dst.Set(src)
			return
		}
		dst.Set(reflect.New(src.Type().Elem()))
		copyValue(dst.Elem(), src.Elem())
	case reflect.Map:
		if src.IsNil() {
			dst.Set(src)
			return
		}
		dst.Set(reflect.MakeMapWithSize(src.Type(), src.Len()))
		for it := src.MapRange(); it.Next(); {
			k := reflect.New(src.Type().Key()).Elem()
			copyValue(k, it.Key())
			v := reflect.New(src.Type().Elem()).Elem()
			copyValue(v, it.Value())
			dst.SetMapIndex(k, v)
		}
	case reflect.Slice:
		if src.IsNil() {
			dst.Set(src)
			return
		}
		dst.Set(reflect.MakeSlice(src.Type(), src.Len(), src.Len()))
		for i := 0; i < src.Len(); i++ {
			copyValue(dst.Index(i), src.Index(i))
		}
	case reflect.Array:
		for i := 0; i < src.Len(); i++ {
			copyValue(dst.Index(i), src.Index(i))
		}
	case reflect.Interface:
		if src.IsNil() {
			dst.Set(src)
			return
		}
		inner := reflect.New(src.Elem().Type()).Elem()
		copyValue(inner, src.Elem())
		dst.Set(inner)
	case reflect.Struct:
		dst.Set(src)
		t := src.Type()
		for i := 0; i < t.NumField(); i++ {
			if t.Field(i).IsExported() {
				copyValue(dst.Field(i), src.Field(i))
			}
		}
	default:
		dst.Set(src)
	}
}

// cloneOf returns a pointer to a deep copy of *p's value.
func cloneOf(p reflect.Value) reflect.Value {
	c := reflect.New(p.Type().Elem())
	copyValue(c.Elem(), p.Elem())
	return c
}
