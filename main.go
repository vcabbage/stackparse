package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/types"
	"io/ioutil"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/tools/go/packages"
)

func main() {
	p, err := ioutil.ReadAll(os.Stdin)
	if err != nil {
		panic(err)
	}
	parse(string(p))
}

func parse(s string) {
	p := parseAndLoad(s)

	var buf bytes.Buffer

	for _, f := range p.funcs {
		if f.typ == nil {
			fmt.Printf("Unable to find declaration for %q.\n", f.name)
			continue
		}

		for _, call := range f.calls {
			buf.Reset()
			writeFunc(f, call, &buf)
			p.rawLines[call.rawLinesIdx] = buf.String()
		}
	}

	for _, l := range p.rawLines {
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

func parseAndLoad(s string) parsed {
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
		Mode: packages.LoadTypes | packages.LoadSyntax,
	}, loadPatterns...)
	if err != nil {
		panic(err)
	}

	// Match functions to their package and file
	for _, pkg := range pkgs {
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
					delete(byName, f.name)
				case *ast.FuncLit:
					startLine := pkg.Fset.Position(n.Pos()).Line
					endLine := pkg.Fset.Position(n.End()).Line
					for _, f := range byName {
						if f.line < startLine || f.line > endLine {
							continue
						}
						f.typ = n.Type
						delete(byName, f.name)
					}
				}

				return true
			}), astFile)
		}
	}

	var funcs []*fn
	for _, fileToFuncs := range pkgToFileToFuncs {
		for _, fs := range fileToFuncs {
			funcs = append(funcs, fs...)
		}
	}

	return parsed{
		rawLines: rawLines,
		funcs:    funcs,
	}
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
	remainingArgBytes := call.argBytes

	fmt.Fprintf(buf, "%s.", f.pkgName)
	if f.recv != nil {
		buf.WriteByte('(')
		remainingArgBytes = writeArgs(f, f.recv.List, remainingArgBytes, call.moreArgs, buf)
		buf.WriteString(").")
	}
	fmt.Fprintf(buf, "%s(", f.name)

	remainingArgBytes = writeArgs(f, f.typ.Params.List, remainingArgBytes, call.moreArgs, buf)
	buf.WriteByte(')')

	if f.typ.Results != nil {
		fmt.Fprintf(buf, " (")
		writeArgs(f, f.typ.Results.List, remainingArgBytes, call.moreArgs, buf)
		fmt.Fprintf(buf, ")")
	}
}

func writeArgs(f *fn, fields []*ast.Field, argBytes []byte, moreArgs bool, buf *bytes.Buffer) (remainingArgBytes []byte) {
	var idx int
	wordRemaining := int64(len(argBytes)) % int64(wordSize)
Outer:
	for _, field := range fields {
		for _, n := range field.Names {
			if idx != 0 {
				buf.WriteString(", ")
			}

			typ := f.pkg.TypesInfo.Types[field.Type].Type
			size := f.pkg.TypesSizes.Sizeof(typ)

			// handle alignment
			if size > wordRemaining && wordRemaining != int64(wordSize) {
				argBytes = argBytes[wordRemaining:]
				wordRemaining = int64(wordSize)
			}
			if size != 0 && size < int64(wordSize) {
				toAlign := wordRemaining % size
				argBytes = argBytes[toAlign:]
				wordRemaining -= toAlign
			}

			if size > int64(len(argBytes)) {
				if moreArgs {
					buf.WriteString("...")
				} else {
					panic("unexpected size > len(argBytes)")
				}
				break Outer
			}

			b := argBytes[:size]
			if size < wordRemaining {
				wordRemaining = wordRemaining - size
			} else {
				wordRemaining = int64(wordSize) - (size % int64(wordSize))
			}

			argBytes = argBytes[size:]
			fmt.Fprintf(buf, "%s ", n.Name)
			formatType(f.pkg.TypesSizes, typ, b, buf)
			idx++
		}
	}
	return remainingArgBytes
}

// TODO: handle differing arch sizes
func formatType(typeSizes types.Sizes, typ types.Type, b []byte, buf *bytes.Buffer) {
	name := typ.String()
	if strings.HasPrefix(name, "*") {
		buf.WriteByte('*')
	}
	if idx := strings.LastIndexByte(name, '.'); idx != -1 {
		name = name[idx+1:]
	}
	buf.WriteString(name)

	switch typ := typ.Underlying().(type) {
	case *types.Array:
		l := int(typ.Len())
		elem := typ.Elem()
		elemSize := typeSizes.Sizeof(elem)

		buf.WriteByte('[')
		for i := 0; i < l; i++ {
			if i != 0 {
				buf.WriteString(", ")
			}
			formatType(typeSizes, elem, b[:elemSize], buf)
			b = b[elemSize:]
		}
		buf.WriteByte(']')
	case *types.Basic:
		switch typ.Kind() {
		case types.Bool:
			t := *(*bool)(unsafe.Pointer(&b[0]))
			if t {
				buf.WriteString("(true)")
			} else {
				buf.WriteString("(false)")
			}
		case types.Int:
			t := *(*int)(unsafe.Pointer(&b[0]))
			fmt.Fprintf(buf, "(%v)", t)
		case types.Int8:
			t := *(*int8)(unsafe.Pointer(&b[0]))
			fmt.Fprintf(buf, "(%v)", t)
		case types.Int16:
			t := *(*int16)(unsafe.Pointer(&b[0]))
			fmt.Fprintf(buf, "(%v)", t)
		case types.Int32:
			t := *(*int32)(unsafe.Pointer(&b[0]))
			fmt.Fprintf(buf, "(%v)", t)
		case types.Int64:
			t := *(*int64)(unsafe.Pointer(&b[0]))
			fmt.Fprintf(buf, "(%v)", t)
		case types.Uint:
			t := *(*uint)(unsafe.Pointer(&b[0]))
			fmt.Fprintf(buf, "(%v)", t)
		case types.Uint8:
			t := *(*uint8)(unsafe.Pointer(&b[0]))
			fmt.Fprintf(buf, "(%v)", t)
		case types.Uint16:
			t := *(*uint16)(unsafe.Pointer(&b[0]))
			fmt.Fprintf(buf, "(%v)", t)
		case types.Uint32:
			t := *(*uint32)(unsafe.Pointer(&b[0]))
			fmt.Fprintf(buf, "(%v)", t)
		case types.Uint64:
			t := *(*uint64)(unsafe.Pointer(&b[0]))
			fmt.Fprintf(buf, "(%v)", t)
		case types.Uintptr:
			t := *(*uintptr)(unsafe.Pointer(&b[0]))
			fmt.Fprintf(buf, "(%v)", t)
		case types.Float32:
			t := *(*float32)(unsafe.Pointer(&b[0]))
			fmt.Fprintf(buf, "(%v)", t)
		case types.Float64:
			t := *(*float64)(unsafe.Pointer(&b[0]))
			fmt.Fprintf(buf, "(%v)", t)
		case types.Complex64:
			t := *(*complex64)(unsafe.Pointer(&b[0]))
			fmt.Fprintf(buf, "(%v)", t)
		case types.Complex128:
			t := *(*complex128)(unsafe.Pointer(&b[0]))
			fmt.Fprintf(buf, "(%v)", t)
		case types.String:
			t := *(*reflect.StringHeader)(unsafe.Pointer(&b[0]))
			fmt.Fprintf(buf, "{data: %s, len: %d}", formatPtr(b[:wordSize]), t.Len)
		case types.UnsafePointer:
			buf.WriteString("(" + formatPtr(b) + ")")
		default:
			panic("unhandled basic type: " + typ.String())
		}
	case *types.Chan:
		buf.WriteString("(" + formatPtr(b) + ")")
	case *types.Interface:
		// interfaces are two pointers: type and data
		fmt.Fprintf(buf, "{type: %s, data: %s}", formatPtr(b[:wordSize]), b[wordSize:])
	case *types.Map:
		buf.WriteString("(" + formatPtr(b) + ")")
	case *types.Pointer:
		buf.WriteString("(" + formatPtr(b) + ")")
	case *types.Signature:
		buf.WriteString("(" + formatPtr(b) + ")")
	case *types.Slice:
		t := *(*reflect.SliceHeader)(unsafe.Pointer(&b[0]))
		fmt.Fprintf(buf, "{data: %s, len: %d, cap: %d}", b[:wordSize], t.Len, t.Cap)
	case *types.Struct:
		fields := make([]*types.Var, typ.NumFields())
		for i := range fields {
			fields[i] = typ.Field(i)
		}
		offsets := typeSizes.Offsetsof(fields)

		buf.WriteRune('{')
		for i, field := range fields {
			if i != 0 {
				buf.WriteString(", ")
			}
			var (
				fieldTyp = field.Type()
				offset   = offsets[i]
				size     = typeSizes.Sizeof(fieldTyp)
			)
			buf.WriteString(field.Name())
			buf.WriteString(": ")
			formatType(typeSizes, fieldTyp, b[offset:offset+size], buf)
		}
		buf.WriteRune('}')
	default:
		panic("unhandled underlying type: " + typ.String())
	}
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
