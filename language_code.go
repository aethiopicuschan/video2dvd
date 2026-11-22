package video2dvd

import (
	"maps"
	"strings"
	"sync"
)

// dvdLanguagePairs lists every ISO 639-1 code with its ISO 639-2
// terminology code and, where it differs, its bibliographic code.
const dvdLanguagePairs = "aa:aar ab:abk ae:ave af:afr ak:aka am:amh an:arg ar:ara as:asm av:ava ay:aym az:aze " +
	"ba:bak be:bel bg:bul bi:bis bm:bam bn:ben bo:bod:tib br:bre bs:bos " +
	"ca:cat ce:che ch:cha co:cos cr:cre cs:ces:cze cu:chu cv:chv cy:cym:wel " +
	"da:dan de:deu:ger dv:div dz:dzo ee:ewe el:ell:gre en:eng eo:epo es:spa et:est eu:eus:baq " +
	"fa:fas:per ff:ful fi:fin fj:fij fo:fao fr:fra:fre fy:fry " +
	"ga:gle gd:gla gl:glg gn:grn gu:guj gv:glv ha:hau he:heb hi:hin ho:hmo hr:hrv ht:hat hu:hun hy:hye:arm hz:her " +
	"ia:ina id:ind ie:ile ig:ibo ii:iii ik:ipk io:ido is:isl:ice it:ita iu:iku ja:jpn jv:jav " +
	"ka:kat:geo kg:kon ki:kik kj:kua kk:kaz kl:kal km:khm kn:kan ko:kor kr:kau ks:kas ku:kur kv:kom kw:cor ky:kir " +
	"la:lat lb:ltz lg:lug li:lim ln:lin lo:lao lt:lit lu:lub lv:lav " +
	"mg:mlg mh:mah mi:mri:mao mk:mkd:mac ml:mal mn:mon mr:mar ms:msa:may mt:mlt my:mya:bur " +
	"na:nau nb:nob nd:nde ne:nep ng:ndo nl:nld:dut nn:nno no:nor nr:nbl nv:nav ny:nya " +
	"oc:oci oj:oji om:orm or:ori os:oss pa:pan pi:pli pl:pol ps:pus pt:por qu:que " +
	"rm:roh rn:run ro:ron:rum ru:rus rw:kin " +
	"sa:san sc:srd sd:snd se:sme sg:sag si:sin sk:slk:slo sl:slv sm:smo sn:sna so:som sq:sqi:alb sr:srp ss:ssw st:sot su:sun sv:swe sw:swa " +
	"ta:tam te:tel tg:tgk th:tha ti:tir tk:tuk tl:tgl tn:tsn to:ton tr:tur ts:tso tt:tat tw:twi ty:tah " +
	"ug:uig uk:ukr ur:urd uz:uzb ve:ven vi:vie vo:vol wa:wln wo:wol xh:xho yi:yid yo:yor za:zha zh:zho:chi zu:zul"

// dvdLanguageAliases maps further language codes to the ISO 639-1 code of
// the language they belong to: individual languages of a macrolanguage,
// Filipino (written like Tagalog), and the codes ISO 639-1 replaced.
var dvdLanguageAliases = map[string]string{
	"cmn": "zh", "yue": "zh", "wuu": "zh", "hak": "zh", "nan": "zh",
	"arb": "ar", "pes": "fa", "zsm": "ms", "ekk": "et", "lvs": "lv",
	"fil": "tl",
	"iw":  "he", "in": "id", "ji": "yi", "jw": "jv", "mo": "ro",
}

var dvdLanguageCodes = sync.OnceValue(func() map[string]string {
	codes := map[string]string{}
	for pair := range strings.FieldsSeq(dvdLanguagePairs) {
		parts := strings.Split(pair, ":")
		for _, code := range parts {
			codes[code] = parts[0]
		}
	}
	maps.Copy(codes, dvdLanguageAliases)
	return codes
})

// DVDLanguageCode returns the two-letter ISO 639-1 code that DVD-Video
// stores for a language, given a BCP 47 tag ("ja", "pt-BR", "zh-Hant"), an
// ISO 639-2 code of either form ("jpn", "ger", "deu") or an ISO 639-1 code.
// It returns "" for an undetermined language ("und", "mul", "zxx") and for a
// language that ISO 639-1 has no code for.
func DVDLanguageCode(language string) string {
	primary, _, _ := strings.Cut(strings.TrimSpace(language), "-")
	primary, _, _ = strings.Cut(primary, "_")
	return dvdLanguageCodes()[strings.ToLower(primary)]
}
