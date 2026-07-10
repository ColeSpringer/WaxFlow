package testutil

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Vector is one SHA-256-pinned external conformance vector. Pinning is
// both reproducibility and supply-chain hygiene: a changed upstream file
// fails loudly instead of silently changing what the suite verifies.
type Vector struct {
	// Name is the path under testdata/vectors/ once fetched.
	Name string
	// URL is the upstream source.
	URL string
	// SHA256 is the hex digest the download must match.
	SHA256 string
}

// Vectors lists every pinned vector, fetched by `make verify-vectors`
// (CI-cached, never committed). The list grows with the codecs: the IETF
// FLAC suite is in; MP3/LAME gapless fixtures and opus_testvectors join
// with their decoders. Committed fixtures stay tiny and live directly
// under testdata/.
//
// The FLAC entries are the complete IETF decoder testbench
// (ietf-wg-cellar/flac-test-files) pinned at commit aa7b0c6: the 64-file
// subset suite (bit-exactness gate), the uncommon set (32-bit, extreme
// rates and block sizes, mid-stream format changes), and the faulty set
// (deliberately broken files that must fail gracefully).
var Vectors = []Vector{
	{Name: "flac/subset/01 - blocksize 4096.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/01%20-%20blocksize%204096.flac", SHA256: "02c7e60ae7788c2cb6898a0a46985d759f1e62482ddd965a374dfd09a2a8390f"},
	{Name: "flac/subset/02 - blocksize 4608.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/02%20-%20blocksize%204608.flac", SHA256: "f4e97914fb9a8cd95b9be90c9970f6d553d76ded55a7a443dbd0d5dfa7bd2819"},
	{Name: "flac/subset/03 - blocksize 16.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/03%20-%20blocksize%2016.flac", SHA256: "164d0747e8c4855407990182b784fcd7475da228bf20a6cff67e947c683959b3"},
	{Name: "flac/subset/04 - blocksize 192.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/04%20-%20blocksize%20192.flac", SHA256: "0f9f7e2fc262e16e8cd92de3e8869d13197a5e9c3ef7239f8ee749cbd2689980"},
	{Name: "flac/subset/05 - blocksize 254.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/05%20-%20blocksize%20254.flac", SHA256: "8965c9616f769573d3bf8ef5b92ee5a2827e797c098a9436a5e3366feb9c9ae2"},
	{Name: "flac/subset/06 - blocksize 512.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/06%20-%20blocksize%20512.flac", SHA256: "ba705cc365abf5b8bd6b4d4392a2668ae395844f82d04e74be3e19b60cb03260"},
	{Name: "flac/subset/07 - blocksize 725.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/07%20-%20blocksize%20725.flac", SHA256: "ffc15a2ead7a6651e2cd36fa886edcffffc32e831077dce852fb7df6f131d2b3"},
	{Name: "flac/subset/08 - blocksize 1000.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/08%20-%20blocksize%201000.flac", SHA256: "fa189c885be140510348f18899fe151a903f0199d502982ecc7863cb85810ba6"},
	{Name: "flac/subset/09 - blocksize 1937.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/09%20-%20blocksize%201937.flac", SHA256: "8786ea66bf5d0083fcf760b874216ac566d0b2688ba58e5711f46525841a0028"},
	{Name: "flac/subset/10 - blocksize 2304.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/10%20-%20blocksize%202304.flac", SHA256: "8ec67465e9041373a67f2440220825ba9d7706789584bffbbb3f109424df25ee"},
	{Name: "flac/subset/11 - partition order 8.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/11%20-%20partition%20order%208.flac", SHA256: "07596315fc9b297702984deab49041f4695e1482ec3e1d081888d1bf13198e53"},
	{Name: "flac/subset/12 - qlp precision 15 bit.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/12%20-%20qlp%20precision%2015%20bit.flac", SHA256: "d33731350269929542c6e0a788c1b9a27eb91dcb3a20f5f960b8569be012c6fa"},
	{Name: "flac/subset/13 - qlp precision 2 bit.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/13%20-%20qlp%20precision%202%20bit.flac", SHA256: "b60e113a6a3b97d7a3c48be90913ced8c85f1ee332c6c5dd5a79b46cdc38108a"},
	{Name: "flac/subset/14 - wasted bits.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/14%20-%20wasted%20bits.flac", SHA256: "58fa05681bd646168ea2a19bc9a6f4ee9d6de295fd832c1f70da1bdbf32d74cb"},
	{Name: "flac/subset/15 - only verbatim subframes.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/15%20-%20only%20verbatim%20subframes.flac", SHA256: "80459f8ad5a9c7edf081a10f9269c55c29136749021e5a55483bc173aaaa82b0"},
	{Name: "flac/subset/16 - partition order 8 containing escaped partitions.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/16%20-%20partition%20order%208%20containing%20escaped%20partitions.flac", SHA256: "5562cff546a1b0d93cea31c0c375c5df65658311de1fc3d322d14b8a2c5ac930"},
	{Name: "flac/subset/17 - all fixed orders.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/17%20-%20all%20fixed%20orders.flac", SHA256: "d6f082cbbbcb03015f861b8fa30cf16708a62b85520905a126da28db29645feb"},
	{Name: "flac/subset/18 - precision search.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/18%20-%20precision%20search.flac", SHA256: "562dd053a92a83bf64baf90ae5f9b3d0af58121c5534b3c05353d77b2ff25aaa"},
	{Name: "flac/subset/19 - samplerate 35467Hz.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/19%20-%20samplerate%2035467Hz.flac", SHA256: "72ee52c82ee46eea28d659392ce75557e66df1051563fde1936d3e5a8fd8d2c2"},
	{Name: "flac/subset/20 - samplerate 39kHz.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/20%20-%20samplerate%2039kHz.flac", SHA256: "9c87394f650fda9d08bc6b4a7c56a4fd56dcac3531913aa536e7d4d576580ee0"},
	{Name: "flac/subset/21 - samplerate 22050Hz.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/21%20-%20samplerate%2022050Hz.flac", SHA256: "a808ffa5c3cb99c7bc8268141f2d01fb7ccb4c3ced617fd7909f578fb9363f88"},
	{Name: "flac/subset/22 - 12 bit per sample.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/22%20-%2012%20bit%20per%20sample.flac", SHA256: "d9c385129c4253da4c0de34ed5b163b404523aa971773277b8b74ef92f7d4f44"},
	{Name: "flac/subset/23 - 8 bit per sample.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/23%20-%208%20bit%20per%20sample.flac", SHA256: "d72ad54e1d0ebe5d65b1b1addd13ee873d38c58cb3357774a1943ced2d984cf9"},
	{Name: "flac/subset/24 - variable blocksize file created with flake revision 264.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/24%20-%20variable%20blocksize%20file%20created%20with%20flake%20revision%20264.flac", SHA256: "c6664710c935e4d862ab04f146ddd7f6df9e2278b651c17a432934e61488eaf7"},
	{Name: "flac/subset/25 - variable blocksize file created with flake revision 264, modified to create smaller blocks.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/25%20-%20variable%20blocksize%20file%20created%20with%20flake%20revision%20264,%20modified%20to%20create%20smaller%20blocks.flac", SHA256: "df5658969201d6867a1cc64d69e30a8ec833b53f7239345eda7eefff1a4a2ad8"},
	{Name: "flac/subset/26 - variable blocksize file created with CUETools.Flake 2.1.6.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/26%20-%20variable%20blocksize%20file%20created%20with%20CUETools.Flake%202.1.6.flac", SHA256: "355ddbac0f596623f805c3d3ff8cf4ba25d3a13c9d61e63719a8d02ebb647c19"},
	{Name: "flac/subset/27 - old format variable blocksize file created with Flake 0.11.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/27%20-%20old%20format%20variable%20blocksize%20file%20created%20with%20Flake%200.11.flac", SHA256: "21b486a42dbf64c043d57dcd5a038163dc77f5eec398312d994643617ec3c5b6"},
	{Name: "flac/subset/28 - high resolution audio, default settings.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/28%20-%20high%20resolution%20audio,%20default%20settings.flac", SHA256: "62322a5c7821081bae99cc1a3f398f177a17cf9312131a5fbcfcc2ee38539346"},
	{Name: "flac/subset/29 - high resolution audio, blocksize 16384.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/29%20-%20high%20resolution%20audio,%20blocksize%2016384.flac", SHA256: "7551605d8a90a7e74e788b31273493ce5e24059d839b1d44dbad89cf9fb132a5"},
	{Name: "flac/subset/30 - high resolution audio, blocksize 13456.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/30%20-%20high%20resolution%20audio,%20blocksize%2013456.flac", SHA256: "d2e6b73cc0888419d709a03c317f59dfb8d48fcdc29e73fb0c099d5de7d512be"},
	{Name: "flac/subset/31 - high resolution audio, using only 32nd order predictors.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/31%20-%20high%20resolution%20audio,%20using%20only%2032nd%20order%20predictors.flac", SHA256: "caf84fbf740a98e3371a58df03422722a7d6058289b11a291f0b39103ea20133"},
	{Name: "flac/subset/32 - high resolution audio, partition order 8 containing escaped partitions.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/32%20-%20high%20resolution%20audio,%20partition%20order%208%20containing%20escaped%20partitions.flac", SHA256: "479474ac6a101725976aa9a4e94580040d650779da41db2981be03aa41ef7fdd"},
	{Name: "flac/subset/33 - samplerate 192kHz.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/33%20-%20samplerate%20192kHz.flac", SHA256: "4833aafc6a7d652bac05de6d304d3e63cc419a731df995b146d91294062bbcd0"},
	{Name: "flac/subset/34 - samplerate 192kHz, using only 32nd order predictors.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/34%20-%20samplerate%20192kHz,%20using%20only%2032nd%20order%20predictors.flac", SHA256: "b32a7e21114034610fea4d43610980111dc9c40faa315a3111196102a7bac36e"},
	{Name: "flac/subset/35 - samplerate 134560Hz.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/35%20-%20samplerate%20134560Hz.flac", SHA256: "ffe10b26321fdc1a1be31cfd7034a4f98b1c0cb3e32e9c1cc129b7ff4a259177"},
	{Name: "flac/subset/36 - samplerate 384kHz.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/36%20-%20samplerate%20384kHz.flac", SHA256: "63fc6b1ae16fc0285f7053f49ce967087f6a041630ee105d3d67fd8d34e45b7c"},
	{Name: "flac/subset/37 - 20 bit per sample.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/37%20-%2020%20bit%20per%20sample.flac", SHA256: "8d8c0526b7823099974a348a6ae80eff9c7eeb4be4e3da270e8e2e9e45d74b3c"},
	{Name: "flac/subset/38 - 3 channels (3.0).flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/38%20-%203%20channels%20(3.0).flac", SHA256: "44d14d0efa374049fd9f56aaee3a068a30045e62a571df574c7b824ae596a3cf"},
	{Name: "flac/subset/39 - 4 channels (4.0).flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/39%20-%204%20channels%20(4.0).flac", SHA256: "ac670ce52561023dca32f7bd20fbe94c1e1459f06188cd0f870801ea5fb23cee"},
	{Name: "flac/subset/40 - 5 channels (5.0).flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/40%20-%205%20channels%20(5.0).flac", SHA256: "bf616e933caab9b7a5da64f9527390f83f208134d327c72a16225c0779ba0da0"},
	{Name: "flac/subset/41 - 6 channels (5.1).flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/41%20-%206%20channels%20(5.1).flac", SHA256: "c12650fda54a384abf017d4b931491de043eaa7a815ae74c7a9b49d4087d6193"},
	{Name: "flac/subset/42 - 7 channels (6.1).flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/42%20-%207%20channels%20(6.1).flac", SHA256: "07a5d7fc8c4dd74c634b2f0e912a063496f67dd2ba9fbe14f7b5fa37fee8f638"},
	{Name: "flac/subset/43 - 8 channels (7.1).flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/43%20-%208%20channels%20(7.1).flac", SHA256: "9c39ff436f316b3a8e72200c8f38af2d9ef15418c25be555aa621d0c793a6254"},
	{Name: "flac/subset/44 - 8-channel surround, 192kHz, 24 bit, using only 32nd order predictors.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/44%20-%208-channel%20surround,%20192kHz,%2024%20bit,%20using%20only%2032nd%20order%20predictors.flac", SHA256: "1a0e8c17392ae03b2cb04e8c82fcc1f924580fdc978884101faa3e9cf7ed9197"},
	{Name: "flac/subset/45 - no total number of samples set.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/45%20-%20no%20total%20number%20of%20samples%20set.flac", SHA256: "336a18eb7a78f7fc0ab34980348e2895bc3f82db440a2430d9f92e996f889f9a"},
	{Name: "flac/subset/46 - no min-max framesize set.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/46%20-%20no%20min-max%20framesize%20set.flac", SHA256: "9dc39732ce17815832790901b768bb50cd5ff0cd21b28a123c1cabc16ed776cc"},
	{Name: "flac/subset/47 - only STREAMINFO.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/47%20-%20only%20STREAMINFO.flac", SHA256: "9a62c79f634849e74cb2183f9e3a9bd284f51e2591c553008d3e6449967eef85"},
	{Name: "flac/subset/48 - Extremely large SEEKTABLE.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/48%20-%20Extremely%20large%20SEEKTABLE.flac", SHA256: "4417aca6b5f90971c50c28766d2f32b3acaa7f9f9667bd313336242dae8b2531"},
	{Name: "flac/subset/49 - Extremely large PADDING.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/49%20-%20Extremely%20large%20PADDING.flac", SHA256: "7bc44fa2754536279fde4f8fb31d824f43b8d0b3f93d27d055d209682914f20e"},
	{Name: "flac/subset/50 - Extremely large PICTURE.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/50%20-%20Extremely%20large%20PICTURE.flac", SHA256: "1f04f237d74836104993a8072d4223e84a5d3bd76fbc44555c221c7e69a23594"},
	{Name: "flac/subset/51 - Extremely large VORBISCOMMENT.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/51%20-%20Extremely%20large%20VORBISCOMMENT.flac", SHA256: "033160e8124ed287b0b5d615c94ac4139477e47d6e4059b1c19b7141566f5ef9"},
	{Name: "flac/subset/52 - Extremely large APPLICATION.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/52%20-%20Extremely%20large%20APPLICATION.flac", SHA256: "0e45a4f8dbef15cbebdd8dfe690d8ae60e0c6abb596db1270a9161b62a7a3f1c"},
	{Name: "flac/subset/53 - CUESHEET with very many indexes.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/53%20-%20CUESHEET%20with%20very%20many%20indexes.flac", SHA256: "513fad18578f3225fae5de1bda8f700415be6fd8aa1e7af533b5eb796ed2d461"},
	{Name: "flac/subset/54 - 1000x repeating VORBISCOMMENT.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/54%20-%201000x%20repeating%20VORBISCOMMENT.flac", SHA256: "b68dc6644784fac35aa07581be8603a360d1697e07a2265d7eb24001936fd247"},
	{Name: "flac/subset/55 - file 48-53 combined.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/55%20-%20file%2048-53%20combined.flac", SHA256: "a756b460df79b7cc492223f80cda570e4511f2024e5fa0c4d505ba51b86191f6"},
	{Name: "flac/subset/56 - JPG PICTURE.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/56%20-%20JPG%20PICTURE.flac", SHA256: "5cebe7a3710cf8924bd2913854e9ca60b4cd53cfee5a3af0c3c73fddc1888963"},
	{Name: "flac/subset/57 - PNG PICTURE.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/57%20-%20PNG%20PICTURE.flac", SHA256: "c6abff7f8bb63c2821bd21dd9052c543f10ba0be878e83cb419c248f14f72697"},
	{Name: "flac/subset/58 - GIF PICTURE.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/58%20-%20GIF%20PICTURE.flac", SHA256: "7c2b1a963a665847167a7275f9924f65baeb85c21726c218f61bf3f803f301c8"},
	{Name: "flac/subset/59 - AVIF PICTURE.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/59%20-%20AVIF%20PICTURE.flac", SHA256: "7395d02bf8d9533dc554cce02dee9de98c77f8731a45f62d0a243bd0d6f9a45c"},
	{Name: "flac/subset/60 - mono audio.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/60%20-%20mono%20audio.flac", SHA256: "20539fbbd6ae28cea2b2182a4c60c6c70cbb710a136199fc83200c4e5fa00b1c"},
	{Name: "flac/subset/61 - predictor overflow check, 16-bit.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/61%20-%20predictor%20overflow%20check,%2016-bit.flac", SHA256: "9b15791a8b8ca14a6b9a2374ecf7c34be25bc7b9e64fa600eb5f7608194b0b61"},
	{Name: "flac/subset/62 - predictor overflow check, 20-bit.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/62%20-%20predictor%20overflow%20check,%2020-bit.flac", SHA256: "a6b3904e8bcce7a93ad529f9b07e64d06d6647ea7a775ba91490dbacc3c771b7"},
	{Name: "flac/subset/63 - predictor overflow check, 24-bit.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/63%20-%20predictor%20overflow%20check,%2024-bit.flac", SHA256: "d5e072373f2d9e80ca2e49b7dd349ebd4340929143d7e5f2c154a97f04755c38"},
	{Name: "flac/subset/64 - rice partitions with escape code zero.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/subset/64%20-%20rice%20partitions%20with%20escape%20code%20zero.flac", SHA256: "3ad0bf2e9b9c78cd04deb0a8e133f5d088db051e42cbf3ffb15e1b528f009ca6"},
	{Name: "flac/uncommon/01 - changing samplerate.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/uncommon/01%20-%20changing%20samplerate.flac", SHA256: "9fa8f7c57186be8708f42620885a71d4c4871767ff55cfe063eb1cf3bad8d0a5"},
	{Name: "flac/uncommon/02 - increasing number of channels.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/uncommon/02%20-%20increasing%20number%20of%20channels.flac", SHA256: "fe70d5e324bea56e9569d4ff9d4b6fa9c33d3f763e7fec5b1f27904e648a0482"},
	{Name: "flac/uncommon/03 - decreasing number of channels.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/uncommon/03%20-%20decreasing%20number%20of%20channels.flac", SHA256: "8e3c451d835cd9a0ef8914e027cf93c98985bd1bb4d7eea59deb15996b97be3b"},
	{Name: "flac/uncommon/04 - changing bitdepth.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/uncommon/04%20-%20changing%20bitdepth.flac", SHA256: "f614382d8bd33b881668f2384dff03478ec2f863cb7069c42120cf8586c145b7"},
	{Name: "flac/uncommon/05 - 32bps audio.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/uncommon/05%20-%2032bps%20audio.flac", SHA256: "de71cbd81f0be1743fc8264f159a521dfc1d92a5e2056c30a64d66001e055eec"},
	{Name: "flac/uncommon/06 - samplerate 768kHz.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/uncommon/06%20-%20samplerate%20768kHz.flac", SHA256: "3e773546d57f7aa7d126a225f8db152a34196725b71ffb76bf2fe05bad6fbb9f"},
	{Name: "flac/uncommon/07 - 15 bit per sample.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/uncommon/07%20-%2015%20bit%20per%20sample.flac", SHA256: "7defaacc8d1bcd3201081d7054de5df1665d17f79851c35b3c6360401d30aa71"},
	{Name: "flac/uncommon/08 - blocksize 65535.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/uncommon/08%20-%20blocksize%2065535.flac", SHA256: "2c0825fc761db080dbfdc11ea2d2b02ee414c9ed83c6240ffbac62db22d08714"},
	{Name: "flac/uncommon/09 - Rice partition order 15.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/uncommon/09%20-%20Rice%20partition%20order%2015.flac", SHA256: "33ee6f02cc9d930dd421e02b716bc2d9d520c3facdf98030eb09c303227f8626"},
	{Name: "flac/uncommon/10 - file starting at frame header.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/uncommon/10%20-%20file%20starting%20at%20frame%20header.flac", SHA256: "d95f63e8101320f5ac7ffe249bc429a209eb0e10996a987301eaa63386a8faa1"},
	{Name: "flac/uncommon/11 - file starting with unparsable data.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/uncommon/11%20-%20file%20starting%20with%20unparsable%20data.flac", SHA256: "40c58b833fb07f0de41259d83cde78211e3faaf9e5f844aed43fa52c18435d2f"},
	{Name: "flac/faulty/01 - wrong max blocksize.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/faulty/01%20-%20wrong%20max%20blocksize.flac", SHA256: "7abd0c6062b33949cc646bc887b5267d698ec8aff9cd8d8c2f270d40b7b63615"},
	{Name: "flac/faulty/02 - wrong maximum framesize.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/faulty/02%20-%20wrong%20maximum%20framesize.flac", SHA256: "1e7f0cb5e19b984d549beea65a9f074cc7693ff594e6ae4685bca43b48dc3143"},
	{Name: "flac/faulty/03 - wrong bit depth.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/faulty/03%20-%20wrong%20bit%20depth.flac", SHA256: "6d3258785ae23fe90958a7bd8eab8443d07d28f456b5d49b908772fa2f803a22"},
	{Name: "flac/faulty/04 - wrong number of channels.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/faulty/04%20-%20wrong%20number%20of%20channels.flac", SHA256: "5410a65f762a6c97a2f8c51fb0354bbde564f35d07ef95fee60e8a715d110efc"},
	{Name: "flac/faulty/05 - wrong total number of samples.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/faulty/05%20-%20wrong%20total%20number%20of%20samples.flac", SHA256: "92f98457511f2f9413445f2fb3e236ae92eab86c38e0082dfbe9dd96d01ba92c"},
	{Name: "flac/faulty/06 - missing streaminfo metadata block.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/faulty/06%20-%20missing%20streaminfo%20metadata%20block.flac", SHA256: "53aed5e7fde7a652b82ba06a8382b2612b02ebbde7b0d2016276644d17cc76cd"},
	{Name: "flac/faulty/07 - other metadata blocks preceding streaminfo metadata block.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/faulty/07%20-%20other%20metadata%20blocks%20preceding%20streaminfo%20metadata%20block.flac", SHA256: "6d46725991ba5da477187fde7709ea201c399d00027257c365d7301226d851ea"},
	{Name: "flac/faulty/08 - blocksize 65536.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/faulty/08%20-%20blocksize%2065536.flac", SHA256: "f6e3b6b72e86f259fcdafc50486b8bf9d4d85b193e18e6e4aa63a6e1ca9e7928"},
	{Name: "flac/faulty/09 - blocksize 1.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/faulty/09%20-%20blocksize%201.flac", SHA256: "4a1e703e9ac7dc67ac82dc74087ba1d62a0f13e89d11e020e01e26306a0102af"},
	{Name: "flac/faulty/10 - invalid vorbis comment metadata block.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/faulty/10%20-%20invalid%20vorbis%20comment%20metadata%20block.flac", SHA256: "c79b0514a61634035a5653c5493797bbd1fcc78982116e4d429630e9e462d29b"},
	{Name: "flac/faulty/11 - incorrect metadata block length.flac", URL: "https://raw.githubusercontent.com/ietf-wg-cellar/flac-test-files/aa7b0c6cf32994c106ae517a08134c28a96ff5b2/faulty/11%20-%20incorrect%20metadata%20block%20length.flac", SHA256: "3732151ba8c4e66a785165aa75a444aad814c16807ddc97b793811376acacfd6"},
	// The official Opus conformance vectors: testvector01-12 in opus_demo
	// bitstream form plus their reference decodes at both rates (stereo
	// testvectorNN.dec, mono testvectorNNm.dec), pinned as the upstream
	// tarball and extracted by the opus conformance test. This is the RFC
	// 8251 tarball: the bitstreams are byte-identical to the original 2012
	// RFC 6716 tarball, but the references were regenerated after RFC 8251's
	// normative decoder changes (the 2012 hybrid/transition references are
	// stale: current libopus fails 05/06/12 against them) and mono
	// references were added.
	{Name: "opus/opus_testvectors-rfc8251.tar.gz", URL: "https://opus-codec.org/static/testvectors/opus_testvectors-rfc8251.tar.gz", SHA256: "6b26a22f9ba87b2b836906a9bb7afec5f8e54d49553b1200382520ee6fedfa55"},
	// The libopus source release the Opus work was ported from. It is pinned
	// here so `make opus-tools` can build the reference tools opus_demo and
	// opus_compare, the encoder-quality oracle (docs/quality-gates.md): a
	// test-time oracle only, never a runtime dependency, exactly like ffmpeg.
	{Name: "opus/opus-1.6.1.tar.gz", URL: "https://downloads.xiph.org/releases/opus/opus-1.6.1.tar.gz", SHA256: "6ffcb593207be92584df15b32466ed64bbec99109f007c82205f0194572411a1"},
	// The Opus encoder-quality corpus: the first 20 reference clips (a
	// bias-free fixed prefix) of the 30-sample Hydrogenaudio 2011 public
	// multiformat listening test, which mixed 15 known-difficult samples from
	// prior HA tests with 15 organizer-selected ones spanning music, speech,
	// transient, and tonal material. The clips are exactly the encoder-gate
	// shape (48 kHz / 16-bit / stereo WAV, 7-30 s), were hand-picked for
	// codec evaluation, and Xiph has hosted them with upstream SHA256SUMS
	// since 2011. They are short fair-use excerpts: fetched at test time,
	// never committed or redistributed.
	{Name: "opus/corpus/sample01r.wav", URL: "https://media.xiph.org/audio/HA_2011/sample01r.wav", SHA256: "fd843f1288aa3b6d5e3adfa8cf7b0ee5f0aef2cc5ce5bbf3cf01203ecfc84abf"},
	{Name: "opus/corpus/sample02r.wav", URL: "https://media.xiph.org/audio/HA_2011/sample02r.wav", SHA256: "ca8a4603211dd9bd7476d99811bed773501dc250804f4c07e7c68e95ab7dd53c"},
	{Name: "opus/corpus/sample03r.wav", URL: "https://media.xiph.org/audio/HA_2011/sample03r.wav", SHA256: "479834bbaf3bb54145e3ee1b32e575f3620400dcdb81fb6fe188e7a73d8f4f74"},
	{Name: "opus/corpus/sample04r.wav", URL: "https://media.xiph.org/audio/HA_2011/sample04r.wav", SHA256: "1d2647185e5da6ced1912a8c5dd9c7c1f3f6029b2eb72f37d3ee2afb83177e6d"},
	{Name: "opus/corpus/sample05r.wav", URL: "https://media.xiph.org/audio/HA_2011/sample05r.wav", SHA256: "d60fca97e5732b6f9b65fdb792c3f7c11f6c3feeb558a46ebd4066c811164058"},
	{Name: "opus/corpus/sample06r.wav", URL: "https://media.xiph.org/audio/HA_2011/sample06r.wav", SHA256: "db700dd9097110206c1e3a4ade4a907cb8976f2b6fd4bfc5d7f0fe297449ae74"},
	{Name: "opus/corpus/sample07r.wav", URL: "https://media.xiph.org/audio/HA_2011/sample07r.wav", SHA256: "8a73cdc83ca5438537e81e56c4b19e5a94417179e86c5e49d75139e59a71d2c0"},
	{Name: "opus/corpus/sample08r.wav", URL: "https://media.xiph.org/audio/HA_2011/sample08r.wav", SHA256: "8a3e074dcdbb4c2918751c04f03af824bdb565fdc3197b1abb73ee6d7e255371"},
	{Name: "opus/corpus/sample09r.wav", URL: "https://media.xiph.org/audio/HA_2011/sample09r.wav", SHA256: "672c11e4f85e22ff7ba10e7c33dd1a71c3d4bc57238fc5abee55582bf6d2066d"},
	{Name: "opus/corpus/sample10r.wav", URL: "https://media.xiph.org/audio/HA_2011/sample10r.wav", SHA256: "3e635e89c56b8bf3f6802fbaddce54ea0dd50f17928f22c7dc483668dafe8a9b"},
	{Name: "opus/corpus/sample11r.wav", URL: "https://media.xiph.org/audio/HA_2011/sample11r.wav", SHA256: "1815761739e407802bb7ac9248d67af20d16ca962c66f1b0e4083b1d180cd820"},
	{Name: "opus/corpus/sample12r.wav", URL: "https://media.xiph.org/audio/HA_2011/sample12r.wav", SHA256: "b7b4ac804dded473e8a80519c2e791705fa94977b0b1d8bbe7d3fbe4904ec189"},
	{Name: "opus/corpus/sample13r.wav", URL: "https://media.xiph.org/audio/HA_2011/sample13r.wav", SHA256: "a58082e4e47c8ba2e5f838c1be0e69b4bcfd0a778f20f43594f5a5c119378f30"},
	{Name: "opus/corpus/sample14r.wav", URL: "https://media.xiph.org/audio/HA_2011/sample14r.wav", SHA256: "8f736812cf0551b3aedf19544be8e8a23e2ba71e46280f243f5b9fd7f5e71676"},
	{Name: "opus/corpus/sample15r.wav", URL: "https://media.xiph.org/audio/HA_2011/sample15r.wav", SHA256: "8fb87f1dfc95dfcc996cd983d8bcd3a07652fbc913d4388e7304a9847b22ca75"},
	{Name: "opus/corpus/sample16r.wav", URL: "https://media.xiph.org/audio/HA_2011/sample16r.wav", SHA256: "be02daaaf4999de279ddf7b3429b55cbaf89da8cb4e3fc983625a43a516501cc"},
	{Name: "opus/corpus/sample17r.wav", URL: "https://media.xiph.org/audio/HA_2011/sample17r.wav", SHA256: "d7d5581c33708bd5444c8ec2e52517409845345e04956c243c0a6de91a5e5b62"},
	{Name: "opus/corpus/sample18r.wav", URL: "https://media.xiph.org/audio/HA_2011/sample18r.wav", SHA256: "8c8992f75ad19d4c6bcc08451f10e24ad4cf079edb0f6274678bf45d4346a42d"},
	{Name: "opus/corpus/sample19r.wav", URL: "https://media.xiph.org/audio/HA_2011/sample19r.wav", SHA256: "449a991ff64548a542ead06731df08985144dac90d5331fc80fe072165c6be0a"},
	{Name: "opus/corpus/sample20r.wav", URL: "https://media.xiph.org/audio/HA_2011/sample20r.wav", SHA256: "959d38211f5e6318a7dc47dae8f7f3b3be6d5397c22ee73db17ab376402b6515"},
	// The Opus speech-quality corpus source: the McGill TSP Speech Database
	// 48 kHz set (Peter Kabal, BSD-2-Clause; hosted by the McGill MMSP lab
	// since the late 1990s), ~1400 short studio-recorded Harvard sentences
	// from two dozen speakers as mono 16-bit WAV. The speech gate reads a
	// fixed subset straight from the zip (OpusSpeechCorpus): eight speakers,
	// four female and four male, six lexically-first utterances each,
	// concatenated to one ~15 s item per speaker. Fetched at test time and
	// cached like the tarballs; never committed.
	{Name: "opus/speech/tsp48k.zip", URL: "https://www.mmsp.ece.mcgill.ca/Documents/Data/TSP-Speech-Database/48k.zip", SHA256: "9cfb3a3a13014c8ff90770a5d1923f376da73ac927b9100f09826f60cf06cf43"},
}

// OpusSpeechCorpus returns the speech-gate items: a name per speaker and the
// zip member paths whose decoded audio is concatenated in order.
func OpusSpeechCorpus() map[string][]string {
	speakers := map[string][]string{}
	for _, s := range []struct{ spk, script string }{
		{"FA", "FA01"}, {"FC", "FC13"}, {"FE", "FE25"}, {"FG", "FG37"},
		{"MA", "MA01"}, {"MC", "MC13"}, {"MH", "MH43"}, {"MK", "MK61"},
	} {
		var members []string
		for i := 1; i <= 6; i++ {
			members = append(members, fmt.Sprintf("48k/%s/%s_%02d.wav", s.spk, s.script, i))
		}
		speakers["tsp-"+strings.ToLower(s.spk)] = members
	}
	return speakers
}

// OpusQualityCorpus lists the vector names of the pinned 20-track Opus
// encoder-quality corpus in gate order.
func OpusQualityCorpus() []string {
	names := make([]string, 0, 20)
	for _, v := range Vectors {
		if strings.HasPrefix(v.Name, "opus/corpus/") {
			names = append(names, v.Name)
		}
	}
	return names
}

// VectorsDir returns the on-disk vector cache, testdata/vectors under the
// repository root (located relative to this source file).
func VectorsDir() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("testutil: cannot locate source file for vectors dir")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "vectors")
}

// VectorPath returns the local path of a fetched vector. Tests self-skip
// when it has not been fetched; WAXFLOW_REQUIRE_VECTORS=1 (CI jobs that
// ran `make verify-vectors` first) escalates absence to failure.
func VectorPath(t testing.TB, name string) string {
	t.Helper()
	path := filepath.Join(VectorsDir(), filepath.FromSlash(name))
	if _, err := os.Stat(path); err != nil {
		if os.Getenv("WAXFLOW_REQUIRE_VECTORS") == "1" {
			t.Fatalf("vector %s required by WAXFLOW_REQUIRE_VECTORS=1 but not fetched (run `make verify-vectors`)", name)
		}
		t.Skipf("vector %s not fetched (run `make verify-vectors`); skipping", name)
	}
	return path
}

// Fetch downloads vectors into dir, verifying each digest. Files already
// present with a matching digest are kept; mismatches are re-downloaded,
// and a mismatched download is an error. Progress goes to w.
func Fetch(w io.Writer, dir string, vectors []Vector) error {
	for _, v := range vectors {
		path := filepath.Join(dir, filepath.FromSlash(v.Name))
		if sum, err := fileSHA256(path); err == nil {
			if sum == v.SHA256 {
				fmt.Fprintf(w, "ok       %s (cached)\n", v.Name)
				continue
			}
			fmt.Fprintf(w, "refetch  %s (digest changed)\n", v.Name)
		}
		if err := fetchOne(path, v); err != nil {
			return fmt.Errorf("fetching %s: %w", v.Name, err)
		}
		fmt.Fprintf(w, "fetched  %s\n", v.Name)
	}
	fmt.Fprintf(w, "%d vector(s) verified in %s\n", len(vectors), dir)
	return nil
}

func fetchOne(path string, v Vector) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	resp, err := http.Get(v.URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", v.URL, resp.Status)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".fetch-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != v.SHA256 {
		return fmt.Errorf("digest mismatch: got %s, pinned %s", got, v.SHA256)
	}
	return os.Rename(tmp.Name(), path)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
