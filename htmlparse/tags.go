package htmlparse

// tagID is an interned element local name. Lookups are exact (case
// sensitive): the tokenizer lowercases HTML tag names and the foreign
// content rules produce the camel-cased SVG names, so "foreignObject" is
// an atom but "foreignobject" is not. tagUnknown marks every other name;
// code that compares unknown names falls back to the string.
//
// This mirrors html5ever's local_name! atoms (web_atoms) restricted to the
// names the tree builder inspects plus common elements.
type tagID uint16

const (
	tagUnknown tagID = iota
	tagA
	tagAbbr
	tagAddress
	tagAnnotationXml
	tagApplet
	tagArea
	tagArticle
	tagAside
	tagAudio
	tagB
	tagBase
	tagBasefont
	tagBdi
	tagBdo
	tagBgsound
	tagBig
	tagBlockquote
	tagBody
	tagBr
	tagButton
	tagCanvas
	tagCaption
	tagCenter
	tagCite
	tagCode
	tagCol
	tagColgroup
	tagData
	tagDatalist
	tagDd
	tagDel
	tagDesc
	tagDetails
	tagDfn
	tagDialog
	tagDir
	tagDiv
	tagDl
	tagDt
	tagEm
	tagEmbed
	tagFieldset
	tagFigcaption
	tagFigure
	tagFont
	tagFooter
	tagForeignObject
	tagForm
	tagFrame
	tagFrameset
	tagH1
	tagH2
	tagH3
	tagH4
	tagH5
	tagH6
	tagHead
	tagHeader
	tagHgroup
	tagHr
	tagHtml
	tagI
	tagIframe
	tagImage
	tagImg
	tagInput
	tagIns
	tagKbd
	tagKeygen
	tagLabel
	tagLegend
	tagLi
	tagLink
	tagListing
	tagMain
	tagMalignmark
	tagMap
	tagMark
	tagMarquee
	tagMath
	tagMenu
	tagMeta
	tagMeter
	tagMglyph
	tagMi
	tagMn
	tagMo
	tagMs
	tagMtext
	tagNav
	tagNobr
	tagNoembed
	tagNoframes
	tagNoscript
	tagObject
	tagOl
	tagOptgroup
	tagOption
	tagOutput
	tagP
	tagParam
	tagPicture
	tagPlaintext
	tagPre
	tagProgress
	tagQ
	tagRb
	tagRp
	tagRt
	tagRtc
	tagRuby
	tagS
	tagSamp
	tagSarcasm
	tagScript
	tagSearch
	tagSection
	tagSelect
	tagSelectedcontent
	tagSlot
	tagSmall
	tagSource
	tagSpan
	tagStrike
	tagStrong
	tagStyle
	tagSub
	tagSummary
	tagSup
	tagSvg
	tagTable
	tagTbody
	tagTd
	tagTemplate
	tagTextarea
	tagTfoot
	tagTh
	tagThead
	tagTime
	tagTitle
	tagTr
	tagTrack
	tagTt
	tagU
	tagUl
	tagVar
	tagVideo
	tagWbr
	tagXmp
	numTags
)

var tagNames = [numTags]string{
	tagUnknown:         "",
	tagA:               "a",
	tagAbbr:            "abbr",
	tagAddress:         "address",
	tagAnnotationXml:   "annotation-xml",
	tagApplet:          "applet",
	tagArea:            "area",
	tagArticle:         "article",
	tagAside:           "aside",
	tagAudio:           "audio",
	tagB:               "b",
	tagBase:            "base",
	tagBasefont:        "basefont",
	tagBdi:             "bdi",
	tagBdo:             "bdo",
	tagBgsound:         "bgsound",
	tagBig:             "big",
	tagBlockquote:      "blockquote",
	tagBody:            "body",
	tagBr:              "br",
	tagButton:          "button",
	tagCanvas:          "canvas",
	tagCaption:         "caption",
	tagCenter:          "center",
	tagCite:            "cite",
	tagCode:            "code",
	tagCol:             "col",
	tagColgroup:        "colgroup",
	tagData:            "data",
	tagDatalist:        "datalist",
	tagDd:              "dd",
	tagDel:             "del",
	tagDesc:            "desc",
	tagDetails:         "details",
	tagDfn:             "dfn",
	tagDialog:          "dialog",
	tagDir:             "dir",
	tagDiv:             "div",
	tagDl:              "dl",
	tagDt:              "dt",
	tagEm:              "em",
	tagEmbed:           "embed",
	tagFieldset:        "fieldset",
	tagFigcaption:      "figcaption",
	tagFigure:          "figure",
	tagFont:            "font",
	tagFooter:          "footer",
	tagForeignObject:   "foreignObject",
	tagForm:            "form",
	tagFrame:           "frame",
	tagFrameset:        "frameset",
	tagH1:              "h1",
	tagH2:              "h2",
	tagH3:              "h3",
	tagH4:              "h4",
	tagH5:              "h5",
	tagH6:              "h6",
	tagHead:            "head",
	tagHeader:          "header",
	tagHgroup:          "hgroup",
	tagHr:              "hr",
	tagHtml:            "html",
	tagI:               "i",
	tagIframe:          "iframe",
	tagImage:           "image",
	tagImg:             "img",
	tagInput:           "input",
	tagIns:             "ins",
	tagKbd:             "kbd",
	tagKeygen:          "keygen",
	tagLabel:           "label",
	tagLegend:          "legend",
	tagLi:              "li",
	tagLink:            "link",
	tagListing:         "listing",
	tagMain:            "main",
	tagMalignmark:      "malignmark",
	tagMap:             "map",
	tagMark:            "mark",
	tagMarquee:         "marquee",
	tagMath:            "math",
	tagMenu:            "menu",
	tagMeta:            "meta",
	tagMeter:           "meter",
	tagMglyph:          "mglyph",
	tagMi:              "mi",
	tagMn:              "mn",
	tagMo:              "mo",
	tagMs:              "ms",
	tagMtext:           "mtext",
	tagNav:             "nav",
	tagNobr:            "nobr",
	tagNoembed:         "noembed",
	tagNoframes:        "noframes",
	tagNoscript:        "noscript",
	tagObject:          "object",
	tagOl:              "ol",
	tagOptgroup:        "optgroup",
	tagOption:          "option",
	tagOutput:          "output",
	tagP:               "p",
	tagParam:           "param",
	tagPicture:         "picture",
	tagPlaintext:       "plaintext",
	tagPre:             "pre",
	tagProgress:        "progress",
	tagQ:               "q",
	tagRb:              "rb",
	tagRp:              "rp",
	tagRt:              "rt",
	tagRtc:             "rtc",
	tagRuby:            "ruby",
	tagS:               "s",
	tagSamp:            "samp",
	tagSarcasm:         "sarcasm",
	tagScript:          "script",
	tagSearch:          "search",
	tagSection:         "section",
	tagSelect:          "select",
	tagSelectedcontent: "selectedcontent",
	tagSlot:            "slot",
	tagSmall:           "small",
	tagSource:          "source",
	tagSpan:            "span",
	tagStrike:          "strike",
	tagStrong:          "strong",
	tagStyle:           "style",
	tagSub:             "sub",
	tagSummary:         "summary",
	tagSup:             "sup",
	tagSvg:             "svg",
	tagTable:           "table",
	tagTbody:           "tbody",
	tagTd:              "td",
	tagTemplate:        "template",
	tagTextarea:        "textarea",
	tagTfoot:           "tfoot",
	tagTh:              "th",
	tagThead:           "thead",
	tagTime:            "time",
	tagTitle:           "title",
	tagTr:              "tr",
	tagTrack:           "track",
	tagTt:              "tt",
	tagU:               "u",
	tagUl:              "ul",
	tagVar:             "var",
	tagVideo:           "video",
	tagWbr:             "wbr",
	tagXmp:             "xmp",
}

// lookupTag interns name. switch on string(b) does not allocate.
//
// PORT: match on the &[u8] (or a perfect hash); lookupTag and
// lookupTagString must stay identical.
func lookupTag(b []byte) tagID {
	switch string(b) {
	case "a":
		return tagA
	case "abbr":
		return tagAbbr
	case "address":
		return tagAddress
	case "annotation-xml":
		return tagAnnotationXml
	case "applet":
		return tagApplet
	case "area":
		return tagArea
	case "article":
		return tagArticle
	case "aside":
		return tagAside
	case "audio":
		return tagAudio
	case "b":
		return tagB
	case "base":
		return tagBase
	case "basefont":
		return tagBasefont
	case "bdi":
		return tagBdi
	case "bdo":
		return tagBdo
	case "bgsound":
		return tagBgsound
	case "big":
		return tagBig
	case "blockquote":
		return tagBlockquote
	case "body":
		return tagBody
	case "br":
		return tagBr
	case "button":
		return tagButton
	case "canvas":
		return tagCanvas
	case "caption":
		return tagCaption
	case "center":
		return tagCenter
	case "cite":
		return tagCite
	case "code":
		return tagCode
	case "col":
		return tagCol
	case "colgroup":
		return tagColgroup
	case "data":
		return tagData
	case "datalist":
		return tagDatalist
	case "dd":
		return tagDd
	case "del":
		return tagDel
	case "desc":
		return tagDesc
	case "details":
		return tagDetails
	case "dfn":
		return tagDfn
	case "dialog":
		return tagDialog
	case "dir":
		return tagDir
	case "div":
		return tagDiv
	case "dl":
		return tagDl
	case "dt":
		return tagDt
	case "em":
		return tagEm
	case "embed":
		return tagEmbed
	case "fieldset":
		return tagFieldset
	case "figcaption":
		return tagFigcaption
	case "figure":
		return tagFigure
	case "font":
		return tagFont
	case "footer":
		return tagFooter
	case "foreignObject":
		return tagForeignObject
	case "form":
		return tagForm
	case "frame":
		return tagFrame
	case "frameset":
		return tagFrameset
	case "h1":
		return tagH1
	case "h2":
		return tagH2
	case "h3":
		return tagH3
	case "h4":
		return tagH4
	case "h5":
		return tagH5
	case "h6":
		return tagH6
	case "head":
		return tagHead
	case "header":
		return tagHeader
	case "hgroup":
		return tagHgroup
	case "hr":
		return tagHr
	case "html":
		return tagHtml
	case "i":
		return tagI
	case "iframe":
		return tagIframe
	case "image":
		return tagImage
	case "img":
		return tagImg
	case "input":
		return tagInput
	case "ins":
		return tagIns
	case "kbd":
		return tagKbd
	case "keygen":
		return tagKeygen
	case "label":
		return tagLabel
	case "legend":
		return tagLegend
	case "li":
		return tagLi
	case "link":
		return tagLink
	case "listing":
		return tagListing
	case "main":
		return tagMain
	case "malignmark":
		return tagMalignmark
	case "map":
		return tagMap
	case "mark":
		return tagMark
	case "marquee":
		return tagMarquee
	case "math":
		return tagMath
	case "menu":
		return tagMenu
	case "meta":
		return tagMeta
	case "meter":
		return tagMeter
	case "mglyph":
		return tagMglyph
	case "mi":
		return tagMi
	case "mn":
		return tagMn
	case "mo":
		return tagMo
	case "ms":
		return tagMs
	case "mtext":
		return tagMtext
	case "nav":
		return tagNav
	case "nobr":
		return tagNobr
	case "noembed":
		return tagNoembed
	case "noframes":
		return tagNoframes
	case "noscript":
		return tagNoscript
	case "object":
		return tagObject
	case "ol":
		return tagOl
	case "optgroup":
		return tagOptgroup
	case "option":
		return tagOption
	case "output":
		return tagOutput
	case "p":
		return tagP
	case "param":
		return tagParam
	case "picture":
		return tagPicture
	case "plaintext":
		return tagPlaintext
	case "pre":
		return tagPre
	case "progress":
		return tagProgress
	case "q":
		return tagQ
	case "rb":
		return tagRb
	case "rp":
		return tagRp
	case "rt":
		return tagRt
	case "rtc":
		return tagRtc
	case "ruby":
		return tagRuby
	case "s":
		return tagS
	case "samp":
		return tagSamp
	case "sarcasm":
		return tagSarcasm
	case "script":
		return tagScript
	case "search":
		return tagSearch
	case "section":
		return tagSection
	case "select":
		return tagSelect
	case "selectedcontent":
		return tagSelectedcontent
	case "slot":
		return tagSlot
	case "small":
		return tagSmall
	case "source":
		return tagSource
	case "span":
		return tagSpan
	case "strike":
		return tagStrike
	case "strong":
		return tagStrong
	case "style":
		return tagStyle
	case "sub":
		return tagSub
	case "summary":
		return tagSummary
	case "sup":
		return tagSup
	case "svg":
		return tagSvg
	case "table":
		return tagTable
	case "tbody":
		return tagTbody
	case "td":
		return tagTd
	case "template":
		return tagTemplate
	case "textarea":
		return tagTextarea
	case "tfoot":
		return tagTfoot
	case "th":
		return tagTh
	case "thead":
		return tagThead
	case "time":
		return tagTime
	case "title":
		return tagTitle
	case "tr":
		return tagTr
	case "track":
		return tagTrack
	case "tt":
		return tagTt
	case "u":
		return tagU
	case "ul":
		return tagUl
	case "var":
		return tagVar
	case "video":
		return tagVideo
	case "wbr":
		return tagWbr
	case "xmp":
		return tagXmp
	}
	return tagUnknown
}

// lookupTagString is lookupTag for a string.
func lookupTagString(s string) tagID {
	switch s {
	case "a":
		return tagA
	case "abbr":
		return tagAbbr
	case "address":
		return tagAddress
	case "annotation-xml":
		return tagAnnotationXml
	case "applet":
		return tagApplet
	case "area":
		return tagArea
	case "article":
		return tagArticle
	case "aside":
		return tagAside
	case "audio":
		return tagAudio
	case "b":
		return tagB
	case "base":
		return tagBase
	case "basefont":
		return tagBasefont
	case "bdi":
		return tagBdi
	case "bdo":
		return tagBdo
	case "bgsound":
		return tagBgsound
	case "big":
		return tagBig
	case "blockquote":
		return tagBlockquote
	case "body":
		return tagBody
	case "br":
		return tagBr
	case "button":
		return tagButton
	case "canvas":
		return tagCanvas
	case "caption":
		return tagCaption
	case "center":
		return tagCenter
	case "cite":
		return tagCite
	case "code":
		return tagCode
	case "col":
		return tagCol
	case "colgroup":
		return tagColgroup
	case "data":
		return tagData
	case "datalist":
		return tagDatalist
	case "dd":
		return tagDd
	case "del":
		return tagDel
	case "desc":
		return tagDesc
	case "details":
		return tagDetails
	case "dfn":
		return tagDfn
	case "dialog":
		return tagDialog
	case "dir":
		return tagDir
	case "div":
		return tagDiv
	case "dl":
		return tagDl
	case "dt":
		return tagDt
	case "em":
		return tagEm
	case "embed":
		return tagEmbed
	case "fieldset":
		return tagFieldset
	case "figcaption":
		return tagFigcaption
	case "figure":
		return tagFigure
	case "font":
		return tagFont
	case "footer":
		return tagFooter
	case "foreignObject":
		return tagForeignObject
	case "form":
		return tagForm
	case "frame":
		return tagFrame
	case "frameset":
		return tagFrameset
	case "h1":
		return tagH1
	case "h2":
		return tagH2
	case "h3":
		return tagH3
	case "h4":
		return tagH4
	case "h5":
		return tagH5
	case "h6":
		return tagH6
	case "head":
		return tagHead
	case "header":
		return tagHeader
	case "hgroup":
		return tagHgroup
	case "hr":
		return tagHr
	case "html":
		return tagHtml
	case "i":
		return tagI
	case "iframe":
		return tagIframe
	case "image":
		return tagImage
	case "img":
		return tagImg
	case "input":
		return tagInput
	case "ins":
		return tagIns
	case "kbd":
		return tagKbd
	case "keygen":
		return tagKeygen
	case "label":
		return tagLabel
	case "legend":
		return tagLegend
	case "li":
		return tagLi
	case "link":
		return tagLink
	case "listing":
		return tagListing
	case "main":
		return tagMain
	case "malignmark":
		return tagMalignmark
	case "map":
		return tagMap
	case "mark":
		return tagMark
	case "marquee":
		return tagMarquee
	case "math":
		return tagMath
	case "menu":
		return tagMenu
	case "meta":
		return tagMeta
	case "meter":
		return tagMeter
	case "mglyph":
		return tagMglyph
	case "mi":
		return tagMi
	case "mn":
		return tagMn
	case "mo":
		return tagMo
	case "ms":
		return tagMs
	case "mtext":
		return tagMtext
	case "nav":
		return tagNav
	case "nobr":
		return tagNobr
	case "noembed":
		return tagNoembed
	case "noframes":
		return tagNoframes
	case "noscript":
		return tagNoscript
	case "object":
		return tagObject
	case "ol":
		return tagOl
	case "optgroup":
		return tagOptgroup
	case "option":
		return tagOption
	case "output":
		return tagOutput
	case "p":
		return tagP
	case "param":
		return tagParam
	case "picture":
		return tagPicture
	case "plaintext":
		return tagPlaintext
	case "pre":
		return tagPre
	case "progress":
		return tagProgress
	case "q":
		return tagQ
	case "rb":
		return tagRb
	case "rp":
		return tagRp
	case "rt":
		return tagRt
	case "rtc":
		return tagRtc
	case "ruby":
		return tagRuby
	case "s":
		return tagS
	case "samp":
		return tagSamp
	case "sarcasm":
		return tagSarcasm
	case "script":
		return tagScript
	case "search":
		return tagSearch
	case "section":
		return tagSection
	case "select":
		return tagSelect
	case "selectedcontent":
		return tagSelectedcontent
	case "slot":
		return tagSlot
	case "small":
		return tagSmall
	case "source":
		return tagSource
	case "span":
		return tagSpan
	case "strike":
		return tagStrike
	case "strong":
		return tagStrong
	case "style":
		return tagStyle
	case "sub":
		return tagSub
	case "summary":
		return tagSummary
	case "sup":
		return tagSup
	case "svg":
		return tagSvg
	case "table":
		return tagTable
	case "tbody":
		return tagTbody
	case "td":
		return tagTd
	case "template":
		return tagTemplate
	case "textarea":
		return tagTextarea
	case "tfoot":
		return tagTfoot
	case "th":
		return tagTh
	case "thead":
		return tagThead
	case "time":
		return tagTime
	case "title":
		return tagTitle
	case "tr":
		return tagTr
	case "track":
		return tagTrack
	case "tt":
		return tagTt
	case "u":
		return tagU
	case "ul":
		return tagUl
	case "var":
		return tagVar
	case "video":
		return tagVideo
	case "wbr":
		return tagWbr
	case "xmp":
		return tagXmp
	}
	return tagUnknown
}
