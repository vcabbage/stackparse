package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/types"
	"io"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/tools/go/packages"
)

func main() {
	var buf strings.Builder
	_, err := io.Copy(&buf, os.Stdin)
	if err != nil {
		panic(err)
	}

	lines := parse(buf.String())
	for _, l := range lines {
		println(l)
	}
}

type parsed struct {
	rawLines []string
	funcs    []*fn
}

type fn struct {
	name     string
	pkgName  string
	fileName string
	line     int
	pkg      *packages.Package
	typ      *ast.FuncType
	recv     *ast.FieldList

	calls []call
}

type call struct {
	rawLinesIdx int
	argBytes    []byte
	moreArgs    bool
}

var funcRe = regexp.MustCompile(`^.+\..+\(.*\)$`)

func parse(s string) []string {
	// Parse stacktrace
	var (
		pkgToFileToFuncs = make(map[string]map[string][]*fn)
		funcByPath       = make(map[string]*fn)
		loadPatterns     []string
		rawLines         = strings.Split(s, "\n")
	)
	for i := 0; i < len(rawLines)-1; i++ {
		if !funcRe.MatchString(rawLines[i]) {
			continue
		}
		funcLine := rawLines[i]
		rawIdx := i
		i++
		fileLine := rawLines[i]

		var (
			openIdx     = strings.LastIndexByte(funcLine, '(')
			closeIdx    = strings.LastIndexByte(funcLine, ')')
			funcPath    = funcLine[:openIdx]
			dotFirstIdx = strings.IndexByte(funcPath, '.')
			dotLastIdx  = strings.LastIndexByte(funcPath, '.')
		)
		argBytes, more, err := hexValsToBytes(funcLine[openIdx+1 : closeIdx])
		if err != nil {
			panic(err)
		}

		if fn, ok := funcByPath[funcPath]; ok {
			fn.calls = append(fn.calls, call{
				rawLinesIdx: rawIdx,
				argBytes:    argBytes,
				moreArgs:    more,
			})
			continue
		}

		colonIdx := strings.IndexByte(fileLine, ':')
		fileName := strings.TrimSpace(fileLine[:colonIdx])
		lineStr := fileLine[colonIdx+1:]
		if idx := strings.IndexByte(lineStr, ' '); idx != -1 {
			lineStr = lineStr[:idx]
		}
		lineno, err := strconv.Atoi(lineStr)
		if err != nil {
			panic("unable to parse line number: " + lineStr)
		}

		loadPatterns = append(loadPatterns, "file="+fileName)

		f := &fn{
			name:     funcLine[dotLastIdx+1 : openIdx],
			pkgName:  funcLine[:dotFirstIdx],
			fileName: fileName,
			line:     lineno,
			calls: []call{
				{
					argBytes:    argBytes,
					moreArgs:    more,
					rawLinesIdx: rawIdx,
				},
			},
		}

		filesToFunc, ok := pkgToFileToFuncs[f.pkgName]
		if !ok {
			pkgToFileToFuncs[f.pkgName] = map[string][]*fn{f.fileName: {f}}
		} else {
			filesToFunc[f.fileName] = append(filesToFunc[f.fileName], f)
		}
		funcByPath[funcPath] = f
	}

	// Load all relevant files
	pkgs, err := packages.Load(&packages.Config{
		Mode: packages.LoadTypes | packages.LoadSyntax | packages.LoadImports | packages.LoadAllSyntax,
	}, loadPatterns...)
	if err != nil {
		panic(err)
	}

	// Collect all direct and imported packages.
	pkgMap := make(map[string]*packages.Package)
	for _, pkg := range pkgs {
		pkgMap[pkg.ID] = pkg
		for _, imp := range pkg.Imports {
			pkgMap[imp.ID] = imp
		}
	}

	var buf bytes.Buffer

	// Match functions to their package and file
	for _, pkg := range pkgMap {
		filesToFunc, ok := pkgToFileToFuncs[pkg.Name]
		if !ok {
			continue
		}

		for i := range pkg.CompiledGoFiles {
			funcs, ok := filesToFunc[pkg.CompiledGoFiles[i]]
			if !ok {
				continue
			}
			astFile := pkg.Syntax[i]

			byName := make(map[string]*fn)
			for i, f := range funcs {
				byName[f.name] = f
				funcs[i].pkg = pkg
			}

			ast.Walk(visitorFunc(func(n ast.Node) bool {
				if n == nil {
					return false
				}

				switch n := n.(type) {
				case *ast.FuncDecl:
					f, ok := byName[n.Name.Name]
					if !ok {
						break
					}

					f.typ = n.Type
					f.recv = n.Recv

					for _, call := range f.calls {
						buf.Reset()
						writeFunc(f, call, &buf)
						rawLines[call.rawLinesIdx] = buf.String()
					}
					delete(byName, f.name)
				case *ast.FuncLit:
					startLine := pkg.Fset.Position(n.Pos()).Line
					endLine := pkg.Fset.Position(n.End()).Line
					for _, f := range byName {
						if f.line < startLine || f.line > endLine {
							continue
						}
						f.typ = n.Type

						for _, call := range f.calls {
							buf.Reset()
							writeFunc(f, call, &buf)
							rawLines[call.rawLinesIdx] = buf.String()
						}
						delete(byName, f.name)
					}
				}

				return true
			}), astFile)
		}
	}

	return rawLines
}

const wordSize = unsafe.Sizeof(uintptr(0))

func hexValsToBytes(hexVals string) (_ []byte, more bool, _ error) {
	if hexVals == "" {
		return nil, false, nil
	}

	var b []byte
	for _, val := range strings.Split(hexVals, ", ") {
		if val == "..." {
			more = true
			break
		}
		val := strings.TrimPrefix(val, "0x")
		n, err := strconv.ParseUint(val, 16, int(wordSize)*8)
		if err != nil {
			return nil, false, err
		}
		b = append(b, (*(*[wordSize]byte)(unsafe.Pointer(&n)))[:]...)
	}

	return b, more, nil
}

func writeFunc(f *fn, call call, buf *bytes.Buffer) {
	ar := newArgReader(f.pkg.TypesSizes, call.argBytes, call.moreArgs)

	fmt.Fprintf(buf, "%s.", f.pkgName)
	if f.recv != nil {
		buf.WriteByte('(')
		writeArgs(f, f.recv.List, ar, buf)
		buf.WriteString(").")
	}
	fmt.Fprintf(buf, "%s(", f.name)

	writeArgs(f, f.typ.Params.List, ar, buf)
	buf.WriteByte(')')

	if f.typ.Results != nil {
		fmt.Fprintf(buf, " (")
		writeArgs(f, f.typ.Results.List, ar, buf)
		fmt.Fprintf(buf, ")")
	}
}

func writeArgs(f *fn, fields []*ast.Field, ar *argReader, buf *bytes.Buffer) {
	var idx int
	for _, field := range fields {
		for _, n := range field.Names {
			if idx != 0 {
				buf.WriteString(", ")
			}
			typ := f.pkg.TypesInfo.Types[field.Type].Type

			fmt.Fprintf(buf, "%s ", n.Name)
			ok := formatType(f.pkg.TypesSizes, typ, f.pkg.PkgPath, ar, buf, false)
			if !ok {
				return
			}
			idx++
		}
	}
}

type argReader struct {
	sizes         types.Sizes
	remaining     []byte
	wordRemaining int64
	moreArgs      bool
}

func newArgReader(sizes types.Sizes, argBytes []byte, moreArgs bool) *argReader {
	return &argReader{
		sizes:         sizes,
		remaining:     argBytes,
		wordRemaining: int64(len(argBytes)) % int64(wordSize),
		moreArgs:      moreArgs,
	}
}

func (r *argReader) read(typ types.Type) ([]byte, bool) {
	size := r.sizes.Sizeof(typ)

	// handle alignment
	if size > r.wordRemaining && r.wordRemaining != int64(wordSize) {
		r.remaining = r.remaining[r.wordRemaining:]
		r.wordRemaining = int64(wordSize)
	}
	if size != 0 && size < int64(wordSize) {
		toAlign := r.wordRemaining % size
		r.remaining = r.remaining[toAlign:]
		r.wordRemaining -= toAlign
	}

	if size > int64(len(r.remaining)) {
		// if !r.moreArgs {
		// 	panic("unexpected size > len(argBytes)")
		// }
		return r.remaining, false
	}

	b := r.remaining[:size]
	if size < r.wordRemaining {
		r.wordRemaining = r.wordRemaining - size
	} else {
		r.wordRemaining = int64(wordSize) - (size % int64(wordSize))
	}

	r.remaining = r.remaining[size:]

	return b, true
}

type structReader struct {
	sizes   types.Sizes
	offsets []int64
	idx     int
	b       []byte
}

func newStructReader(sizes types.Sizes, fields []*types.Var, b []byte) *structReader {
	return &structReader{
		sizes:   sizes,
		offsets: sizes.Offsetsof(fields),
		b:       b,
	}
}

func (r *structReader) read(typ types.Type) ([]byte, bool) {
	offset := r.offsets[r.idx]
	r.idx++
	size := r.sizes.Sizeof(typ)

	if int64(len(r.b)) <= offset {
		return nil, false
	}

	if int64(len(r.b)) < offset+size {
		return r.b[offset:], false
	}

	return r.b[offset : offset+size], true
}

type reader interface {
	read(typ types.Type) ([]byte, bool)
}

func writeArgName(typ types.Type, pkgPath string, buf *bytes.Buffer) {
	name := typ.String()

	// remove package path from type name when defined in same package as function
	name = strings.Replace(name, pkgPath+".", "", -1)

	// wrap functions signatures in parens
	if strings.Contains(name, " ") {
		name = "(" + name + ")"
	}

	buf.WriteString(name)
}

// TODO: handle differing arch sizes
func formatType(typeSizes types.Sizes, typ types.Type, pkgPath string, ar reader, buf *bytes.Buffer, suppressTypeName bool) bool {
	// Compound types
	switch utyp := typ.Underlying().(type) {
	case *types.Array:
		if !suppressTypeName {
			writeArgName(typ, pkgPath, buf)
		}

		l := int(utyp.Len())
		elem := utyp.Elem()

		var ok bool

		buf.WriteByte('[')
		for i := 0; i < l; i++ {
			if i != 0 {
				buf.WriteString(", ")
			}
			ok = formatType(typeSizes, elem, pkgPath, ar, buf, true)
			if !ok {
				break
			}
		}
		buf.WriteByte(']')
		return ok

	case *types.Struct:
		if !suppressTypeName {
			writeArgName(typ, pkgPath, buf)
		}

		// TODO: handle incomplete structs
		b, _ := ar.read(typ)

		fields := make([]*types.Var, utyp.NumFields())
		for i := range fields {
			fields[i] = utyp.Field(i)
		}
		sr := newStructReader(typeSizes, fields, b)

		var ok bool
		buf.WriteRune('{')
		for i, field := range fields {
			if i != 0 {
				buf.WriteString(", ")
			}
			buf.WriteString(field.Name())
			buf.WriteString(": ")

			fieldTyp := field.Type()
			ok = formatType(typeSizes, fieldTyp, pkgPath, sr, buf, false)
			if !ok {
				break
			}
		}
		buf.WriteRune('}')

		return ok

	case *types.Interface:
		// interfaces are two pointers: type and data
		b, ok := ar.read(typ)
		if !ok {
			buf.WriteString("...")
			return false
		}

		if !suppressTypeName {
			writeArgName(typ, pkgPath, buf)
		}
		fmt.Fprintf(buf, "{type: %s, data: %s}",
			formatPtr(b[:wordSize]),
			formatPtr(b[wordSize:]),
		)
		return true

	case *types.Slice:
		b, ok := ar.read(typ)
		if !ok {
			buf.WriteString("...")
			return false
		}

		if !suppressTypeName {
			writeArgName(typ, pkgPath, buf)
		}

		t := *(*reflect.SliceHeader)(unsafe.Pointer(&b[0]))
		fmt.Fprintf(buf, "{data: %s, len: %d, cap: %d}", formatPtr(b[:wordSize]), t.Len, t.Cap)
		return true

	case *types.Basic:
		switch utyp.Kind() {
		case types.String:
			b, ok := ar.read(typ)
			if !ok {
				buf.WriteString("...")
				return false
			}

			if !suppressTypeName {
				writeArgName(typ, pkgPath, buf)
			}

			t := *(*reflect.StringHeader)(unsafe.Pointer(&b[0]))
			fmt.Fprintf(buf, "{data: %s, len: %d}", formatPtr(b[:wordSize]), t.Len)
			return true
		}
	}

	b, ok := ar.read(typ)
	if !ok {
		buf.WriteString("...")
		return false
	}

	if !suppressTypeName {
		writeArgName(typ, pkgPath, buf)
		buf.WriteRune('(')
	}
	switch typ := typ.Underlying().(type) {
	case *types.Map,
		*types.Pointer,
		*types.Chan,
		*types.Signature:
		buf.WriteString(formatPtr(b))
	case *types.Basic:
		switch typ.Kind() {
		case types.Bool:
			t := *(*bool)(unsafe.Pointer(&b[0]))
			if t {
				buf.WriteString("true")
			} else {
				buf.WriteString("false")
			}
		case types.Int:
			t := *(*int)(unsafe.Pointer(&b[0]))
			fmt.Fprintf(buf, "%v", t)
		case types.Int8:
			t := *(*int8)(unsafe.Pointer(&b[0]))
			fmt.Fprintf(buf, "%v", t)
		case types.Int16:
			t := *(*int16)(unsafe.Pointer(&b[0]))
			fmt.Fprintf(buf, "%v", t)
		case types.Int32:
			t := *(*int32)(unsafe.Pointer(&b[0]))
			fmt.Fprintf(buf, "%v", t)
		case types.Int64:
			t := *(*int64)(unsafe.Pointer(&b[0]))
			fmt.Fprintf(buf, "%v", t)
		case types.Uint:
			t := *(*uint)(unsafe.Pointer(&b[0]))
			fmt.Fprintf(buf, "%v", t)
		case types.Uint8:
			t := *(*uint8)(unsafe.Pointer(&b[0]))
			fmt.Fprintf(buf, "%v", t)
		case types.Uint16:
			t := *(*uint16)(unsafe.Pointer(&b[0]))
			fmt.Fprintf(buf, "%v", t)
		case types.Uint32:
			t := *(*uint32)(unsafe.Pointer(&b[0]))
			fmt.Fprintf(buf, "%v", t)
		case types.Uint64:
			t := *(*uint64)(unsafe.Pointer(&b[0]))
			fmt.Fprintf(buf, "%v", t)
		case types.Uintptr:
			t := *(*uintptr)(unsafe.Pointer(&b[0]))
			fmt.Fprintf(buf, "%v", t)
		case types.Float32:
			t := *(*float32)(unsafe.Pointer(&b[0]))
			fmt.Fprintf(buf, "%v", t)
		case types.Float64:
			t := *(*float64)(unsafe.Pointer(&b[0]))
			fmt.Fprintf(buf, "%v", t)
		case types.Complex64:
			t := *(*complex64)(unsafe.Pointer(&b[0]))
			fmt.Fprintf(buf, "%v", t)
		case types.Complex128:
			t := *(*complex128)(unsafe.Pointer(&b[0]))
			fmt.Fprintf(buf, "%v", t)
		case types.UnsafePointer:
			buf.WriteString(formatPtr(b))
		default:
			panic("unhandled basic type: " + typ.String())
		}
	default:
		panic("unhandled underlying type: " + typ.String())
	}
	if !suppressTypeName {
		buf.WriteByte(')')
	}
	return true
}

func formatPtr(b []byte) string {
	p := *(*uintptr)(unsafe.Pointer(&b[0]))
	if p == 0 {
		return "nil"
	}
	return fmt.Sprintf("%#x", p)
}

type visitorFunc func(ast.Node) bool

func (vf visitorFunc) Visit(n ast.Node) ast.Visitor {
	if vf(n) {
		return vf
	} else {
		return nil
	}
}
