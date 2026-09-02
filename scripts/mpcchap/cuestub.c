/* See cuetools/cuefile.h: the cue-sheet path of mpcchap is compiled out
 * behind these, so the tool built by `make mpc-tools` serves .ini files. */
#include <stddef.h>
#include "cuetools/cuefile.h"

Cd *cf_parse(char *name, int *format) { (void)name; (void)format; return NULL; }
int cd_get_ntrack(Cd *cd) { (void)cd; return 0; }
Track *cd_get_track(Cd *cd, int i) { (void)cd; (void)i; return NULL; }
Cdtext *track_get_cdtext(Track *track) { (void)track; return NULL; }
long track_get_start(Track *track) { (void)track; return 0; }
char *cdtext_get(int pti, Cdtext *cdtext) { (void)pti; (void)cdtext; return NULL; }
