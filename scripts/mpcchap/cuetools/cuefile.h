/* Stub of the libcuefile API that mpcchap's cue-sheet path links against.
 * The pinned musepack tarball does not ship libcuefile, and only the .ini
 * path is used, so `make mpc-tools` builds mpcchap against this. The
 * declarations are reconstructed from the editor's own calls; the enumerator
 * values are placeholders the stub never consults. Every call reports
 * failure, which mpcchap turns into an input file error. */
#ifndef WAXFLOW_CUESTUB_CUEFILE_H
#define WAXFLOW_CUESTUB_CUEFILE_H

typedef struct Cd Cd;
typedef struct Track Track;
typedef struct Cdtext Cdtext;
enum { UNKNOWN = 0 };
enum { PTI_TITLE, PTI_PERFORMER, PTI_SONGWRITER, PTI_COMPOSER, PTI_ARRANGER, PTI_MESSAGE, PTI_GENRE };
Cd *cf_parse(char *name, int *format);
int cd_get_ntrack(Cd *cd);
Track *cd_get_track(Cd *cd, int i);
Cdtext *track_get_cdtext(Track *track);
long track_get_start(Track *track);
char *cdtext_get(int pti, Cdtext *cdtext);

#endif
