package js

// Mã số của từ (word id): chỉ những từ mà lexer cần nhận dạng. Mọi từ khác là wOther.
const (
	wOther uint8 = iota

	// Từ khoá mà sau đó `/` mở regex (wReturn..wLastRx).
	wReturn
	wTypeof
	wInstanceof
	wIn
	wOf
	wNew
	wDelete
	wThrow
	wCase
	wDo
	wElse
	wYield
	wAwait
	wDefault
	wVoid

	// Các từ gọi mock/glob... (wFirstCallee..wLastCallee liền nhau: dùng để tính "ứng viên callee").
	wMock
	wUnmock
	wDoMock
	wDontMock
	wRequireActual
	wRequireMock
	wCreateMockFromModule
	wSetMock
	wDoUnmock
	wImportActual
	wImportMock
	wResolve
	wContext
	wGlob
	wGlobEager
	wURL

	wImport
	wExport
	wRequire
	wJest
	wVi
	wCache
	wMain
	wExtensions
	wMeta
	wFrom
	wType
	wUrl
	wTrue
	wFalse
	wClass
	wInterface
	wEnum
	wFunction
	wConst
	wLet
	wVar
	wCond // if while with
	wFor
	wMod // static async get set public private protected readonly abstract override accessor
)

const (
	wLastRx      = wVoid
	wFirstCallee = wMock
	wLastCallee  = wURL
)

// wordID nhận dạng từ theo nội dung (không cấp phát).
func wordID(b []byte) uint8 {
	if len(b) < 2 || len(b) > 20 {
		return wOther
	}
	switch string(b) {
	case "return":
		return wReturn
	case "typeof":
		return wTypeof
	case "instanceof":
		return wInstanceof
	case "in":
		return wIn
	case "of":
		return wOf
	case "new":
		return wNew
	case "delete":
		return wDelete
	case "throw":
		return wThrow
	case "case":
		return wCase
	case "do":
		return wDo
	case "else":
		return wElse
	case "yield":
		return wYield
	case "await":
		return wAwait
	case "default":
		return wDefault
	case "void":
		return wVoid
	case "mock":
		return wMock
	case "unmock":
		return wUnmock
	case "doMock":
		return wDoMock
	case "dontMock":
		return wDontMock
	case "requireActual":
		return wRequireActual
	case "requireMock":
		return wRequireMock
	case "createMockFromModule":
		return wCreateMockFromModule
	case "setMock":
		return wSetMock
	case "doUnmock":
		return wDoUnmock
	case "importActual":
		return wImportActual
	case "importMock":
		return wImportMock
	case "resolve":
		return wResolve
	case "context":
		return wContext
	case "glob":
		return wGlob
	case "globEager":
		return wGlobEager
	case "URL":
		return wURL
	case "import":
		return wImport
	case "export":
		return wExport
	case "require":
		return wRequire
	case "jest":
		return wJest
	case "vi":
		return wVi
	case "cache":
		return wCache
	case "main":
		return wMain
	case "extensions":
		return wExtensions
	case "meta":
		return wMeta
	case "from":
		return wFrom
	case "type":
		return wType
	case "url":
		return wUrl
	case "true":
		return wTrue
	case "false":
		return wFalse
	case "class":
		return wClass
	case "interface":
		return wInterface
	case "enum":
		return wEnum
	case "function":
		return wFunction
	case "const":
		return wConst
	case "let":
		return wLet
	case "var":
		return wVar
	case "if", "while", "with":
		return wCond
	case "for":
		return wFor
	case "static", "async", "get", "set", "public", "private", "protected", "readonly", "abstract", "override", "accessor":
		return wMod
	}
	return wOther
}

func jestName(w uint8) bool {
	switch w {
	case wMock, wUnmock, wDoMock, wDontMock, wRequireActual, wRequireMock, wCreateMockFromModule, wSetMock:
		return true
	}
	return false
}

func viName(w uint8) bool {
	switch w {
	case wMock, wDoMock, wUnmock, wDoUnmock, wImportActual, wImportMock:
		return true
	}
	return false
}

// reqProp: thuộc tính hợp lệ sau `require.` (không tính là dùng require như giá trị).
func reqProp(w uint8) bool {
	switch w {
	case wResolve, wContext, wCache, wMain, wExtensions:
		return true
	}
	return false
}

// Bảng lớp ký tự.
var (
	identStart [256]bool
	identPart  [256]bool
	strSpecial [256]bool // trong chuỗi '...' / "...": dấu nháy, \, xuống dòng
	tplSpecial [256]bool // trong template: ` \ $ \n
	rxSpecial  [256]bool // trong regex: \ / [ ] xuống dòng
)

func init() {
	for c := 0; c < 256; c++ {
		b := byte(c)
		alpha := (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_' || b == '$' || b >= 0x80
		digit := b >= '0' && b <= '9'
		identStart[c] = alpha
		identPart[c] = alpha || digit
	}
	for _, b := range []byte{'"', '\'', '\\', '\n', '\r'} {
		strSpecial[b] = true
	}
	for _, b := range []byte{'`', '\\', '$', '\n'} {
		tplSpecial[b] = true
	}
	for _, b := range []byte{'\\', '/', '[', ']', '\n', '\r'} {
		rxSpecial[b] = true
	}
}

// uniSpaceLen trả độ dài (byte) của khoảng trắng Unicode ở đầu b, 0 nếu không phải.
func uniSpaceLen(b []byte) int {
	if len(b) < 2 {
		return 0
	}
	switch b[0] {
	case 0xC2:
		if b[1] == 0xA0 {
			return 2
		}
	case 0xE2:
		if len(b) >= 3 && b[1] == 0x80 {
			if (b[2] >= 0x80 && b[2] <= 0x8A) || b[2] == 0xA8 || b[2] == 0xA9 || b[2] == 0xAF {
				return 3
			}
		}
		if len(b) >= 3 && b[1] == 0x81 && b[2] == 0x9F {
			return 3
		}
	case 0xE3:
		if len(b) >= 3 && b[1] == 0x80 && b[2] == 0x80 {
			return 3
		}
	case 0xEF:
		if len(b) >= 3 && b[1] == 0xBB && b[2] == 0xBF {
			return 3
		}
	case 0xE1:
		if len(b) >= 3 && b[1] == 0x9A && b[2] == 0x80 {
			return 3
		}
	}
	return 0
}
