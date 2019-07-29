package main

import (
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

var funcRe = regexp.MustCompile(`^.+\..+\(.*\)$`)

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

func parse(s string) {
	lines := strings.Split(s, "\n")
	for i := 0; i < len(lines); i++ {
		if !funcRe.MatchString(lines[i]) {
			fmt.Println(lines[i])
			continue
		}
		dot := strings.IndexByte(lines[i], '.')
		pkgName := lines[i][:dot]
		open := strings.IndexByte(lines[i], '(')
		close := strings.IndexByte(lines[i], ')')
		funcName := lines[i][dot+1 : open]
		argBytes, more, err := hexValsToBytes(lines[i][open+1 : close])
		if err != nil {
			panic(err)
		}
		i++

		fileName := strings.TrimSpace(lines[i][:strings.IndexByte(lines[i], ':')])
		fileLine := lines[i][strings.IndexByte(lines[i], ':')+1:]
		if idx := strings.IndexByte(fileLine, ' '); idx != -1 {
			fileLine = fileLine[:idx]
		}
		lineno, err := strconv.Atoi(fileLine)
		if err != nil {
			panic("unable to parse line number")
		}
		// TODO: be more efficient about loading
		pkgs, err := packages.Load(&packages.Config{
			Mode: packages.LoadTypes | packages.LoadSyntax,
		}, "file="+fileName)
		if err != nil {
			panic(err)
		}

		var pkg *packages.Package
		for _, p := range pkgs {
			if p.Name != pkgName {
				continue
			}
			pkg = p
			break
		}
		if pkg == nil {
			fmt.Println("Unable to find package.")
			continue
		}

		var file *ast.File
		for i := range pkg.CompiledGoFiles {
			if fileName != pkg.CompiledGoFiles[i] {
				continue
			}
			file = pkg.Syntax[i]
			break
		}
		if file == nil {
			fmt.Println("Unable to find file.")
			continue
		}

		var (
			fn    *ast.FuncType
			found bool
		)
		ast.Walk(visitorFunc(func(n ast.Node) bool {
			if n == nil {
				return false
			}
			if found {
				return false
			}
			// TODO: stop when found
			switch n := n.(type) {
			case *ast.FuncDecl:
				if n.Name.Name != funcName {
					break
				}

				fn = n.Type
				found = true
				return false
			case *ast.FuncLit:
				fmt.Println(pkg.TypesInfo.Types[n.Type].Type)
				startLine := pkg.Fset.Position(n.Pos()).Line
				endLine := pkg.Fset.Position(n.End()).Line
				if lineno >= startLine && lineno <= endLine {
					fn = n.Type
					found = true
					return false
				}
			}
			return true
		}), file)
		if fn == nil {
			fmt.Printf("Unable to find declaration for %q.\n", funcName)
			continue
		}

		// TODO: receiver
		fmt.Printf("%s.%s(", pkgName, funcName)
		var idx int
		wordRemaining := int64(wordSize)
	Outer:
		for _, v := range fn.Params.List {
			for _, n := range v.Names {
				if idx != 0 {
					fmt.Printf(", ")
				}

				typ := pkg.TypesInfo.Types[v.Type].Type
				size := pkg.TypesSizes.Sizeof(typ)

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
					if more {
						fmt.Printf("...")
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
				var buf strings.Builder // TODO: bytes.Buffer or accept interface
				formatType(pkg.TypesSizes, typ, b, &buf)
				fmt.Printf("%s %s", n.Name, buf.String())
				idx++
			}
		}
		fmt.Printf(")")

		if fn.Results != nil {
			fmt.Printf(" (")
			idx = 0
			for _, v := range fn.Results.List {
				for _, n := range v.Names {
					if idx != 0 {
						fmt.Printf(", ")
					}

					typ := pkg.TypesInfo.Types[v.Type].Type
					size := pkg.TypesSizes.Sizeof(typ)
					b := argBytes[:size]
					argBytes = argBytes[size:]
					revBytes(b)
					var buf strings.Builder // TODO: bytes.Buffer or accept interface
					formatType(pkg.TypesSizes, typ, b, &buf)
					fmt.Printf("%s %s", n.Name, buf.String())
					idx++
				}
			}
			fmt.Printf(")")
		}
		fmt.Printf("\n%s\n", lines[i])
	}
}

// TODO: handle differing arch sizes
func formatType(typeSizes types.Sizes, typ types.Type, b []byte, buf *strings.Builder) {
	name := typ.String()
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

func revBytes(b []byte) {
	start := 0
	end := len(b) - 1
	for start < end {
		b[start], b[end] = b[end], b[start]
		start++
		end--
	}
}

type visitorFunc func(ast.Node) bool

func (vf visitorFunc) Visit(n ast.Node) ast.Visitor {
	if vf(n) {
		return vf
	} else {
		return nil
	}
}
