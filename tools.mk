# The reference tools the differential tests check against, each built from
# pinned source into testdata/tools (CI-cached, never committed): test-time
# oracles only, never a runtime dependency. The Makefile includes this file.
#
# Each build is stamped with what it was built from: this file (every
# recipe and variable here), the pinned archives it unpacks and the sources
# of ours it compiles in. A recipe is a no-op while its binaries are there
# and its stamp still matches, and otherwise starts over from an empty
# directory, so an edited input never keeps an old build, in a local tree or
# in CI's tool cache. cksum is in every POSIX userland and only has to notice
# an edit: vectorfetch has already held each archive to its SHA-256 pin.
# `make <recipe>-stamp` prints a stamp; CI keys its tool cache on them.
tool_stamp = cksum tools.mk $(1)
.PHONY: opus-tools-stamp ape-tools-stamp mpc-tools-stamp

# Build the reference libopus tools (opus_demo + opus_compare), the Opus
# encoder-quality oracle, from the pinned source tarball. Requires a C
# toolchain. Tests that need the tools self-skip until this has run;
# WAXFLOW_REQUIRE_OPUS_TOOLS=1 escalates (the CI differential job sets it).
# The .exe suffix follows the ape recipe's platform detection below, as the
# Musepack recipe does.
OPUS_TOOLS_VERSION := opus-1.6.1
OPUS_TOOLS_DIR := testdata/tools/$(OPUS_TOOLS_VERSION)
OPUS_TOOLS_INPUTS = testdata/vectors/opus/$(OPUS_TOOLS_VERSION).tar.gz
opus-tools:
	@set -e; \
	go run ./internal/testutil/cmd/vectorfetch opus/$(OPUS_TOOLS_VERSION).tar.gz; \
	stamp="$$($(call tool_stamp,$(OPUS_TOOLS_INPUTS)))"; \
	if [ -x "$(OPUS_TOOLS_DIR)/opus_demo$(APE_TOOLS_EXE)" ] && [ -x "$(OPUS_TOOLS_DIR)/opus_compare$(APE_TOOLS_EXE)" ] \
		&& [ "$$(cat $(OPUS_TOOLS_DIR)/.stamp 2>/dev/null)" = "$$stamp" ]; then \
		echo "$(OPUS_TOOLS_DIR)/{opus_demo,opus_compare}$(APE_TOOLS_EXE) are already built"; exit 0; \
	fi; \
	rm -rf testdata/tools/opus-build $(OPUS_TOOLS_DIR); \
	mkdir -p testdata/tools/opus-build $(OPUS_TOOLS_DIR); \
	tar -xzf testdata/vectors/opus/$(OPUS_TOOLS_VERSION).tar.gz -C testdata/tools/opus-build --strip-components=1; \
	( cd testdata/tools/opus-build && ./configure --disable-shared --disable-doc >/dev/null && $(MAKE) -s opus_demo opus_compare >/dev/null ); \
	cp testdata/tools/opus-build/opus_demo$(APE_TOOLS_EXE) testdata/tools/opus-build/opus_compare$(APE_TOOLS_EXE) $(OPUS_TOOLS_DIR)/; \
	rm -rf testdata/tools/opus-build; \
	printf '%s\n' "$$stamp" > $(OPUS_TOOLS_DIR)/.stamp; \
	echo "built $(OPUS_TOOLS_DIR)/{opus_demo,opus_compare}$(APE_TOOLS_EXE)"
opus-tools-stamp:
	@$(call tool_stamp,$(OPUS_TOOLS_INPUTS))

# GNU make defaults CXX to g++, which plenty of hosts do not install even
# with a C++ compiler present (the other tool recipes use $(CC), whose default
# cc is always there). Only the built-in default is repointed, so CXX from the
# environment, the command line or a file still wins.
ifeq ($(origin CXX),default)
CXX := $(shell command -v g++ >/dev/null 2>&1 && echo g++ || echo c++)
endif

# Build the reference Monkey's Audio console tool (`mac`) from the pinned SDK
# source. It is the only APE encoder there is -- ffmpeg decodes the format
# but does not write it, and no distribution packages the tool -- so the APE
# fixtures and the reference-tool differentials come from here. Tests that
# need it self-skip until this has run; WAXFLOW_REQUIRE_MAC=1 escalates.
APE_TOOLS_VERSION := mac-13.45
APE_TOOLS_DIR := testdata/tools/$(APE_TOOLS_VERSION)
# The SDK ships as MAC_<version without its dot>_SDK.zip, so the archive name
# follows from APE_TOOLS_VERSION rather than being a second copy of it.
APE_SDK_ZIP := MAC_$(subst .,,$(APE_TOOLS_VERSION:mac-%=%))_SDK.zip
# The SDK picks its file-I/O backend and its wide-character entry point from a
# platform define, so the build needs one of three spellings. These are
# recursively expanded on purpose: only the ape-tools recipe reads them, and an
# immediate assignment would fork uname on every make invocation.
APE_TOOLS_UNAME = $(shell uname -s)
APE_TOOLS_WINDOWS = $(findstring MINGW,$(APE_TOOLS_UNAME))$(findstring MSYS,$(APE_TOOLS_UNAME))$(findstring CYGWIN,$(APE_TOOLS_UNAME))
APE_TOOLS_CXXFLAGS = $(if $(APE_TOOLS_WINDOWS),-DPLATFORM_WINDOWS -municode,$(if $(findstring Darwin,$(APE_TOOLS_UNAME)),-DPLATFORM_APPLE,-DPLATFORM_LINUX))
APE_TOOLS_IO_SRC = $(if $(APE_TOOLS_WINDOWS),Source/Shared/WinFileIO.cpp,Source/Shared/StdLibFileIO.cpp)
APE_TOOLS_EXE = $(if $(APE_TOOLS_WINDOWS),.exe,)
APE_TOOLS_INPUTS = testdata/vectors/ape/$(APE_SDK_ZIP)
ape-tools:
	@set -e; \
	go run ./internal/testutil/cmd/vectorfetch ape/$(APE_SDK_ZIP); \
	stamp="$$($(call tool_stamp,$(APE_TOOLS_INPUTS)))"; \
	if [ -x "$(APE_TOOLS_DIR)/mac$(APE_TOOLS_EXE)" ] && [ "$$(cat $(APE_TOOLS_DIR)/.stamp 2>/dev/null)" = "$$stamp" ]; then \
		echo "$(APE_TOOLS_DIR)/mac$(APE_TOOLS_EXE) is already built"; exit 0; \
	fi; \
	rm -rf testdata/tools/ape-build $(APE_TOOLS_DIR); \
	mkdir -p testdata/tools/ape-build $(APE_TOOLS_DIR); \
	( cd testdata/tools/ape-build && unzip -q ../../vectors/ape/$(APE_SDK_ZIP) ); \
	( cd testdata/tools/ape-build && $(CXX) -O2 -std=c++11 -w $(APE_TOOLS_CXXFLAGS) \
		-ISource/MACLib -ISource/MACLib/Old -ISource/Shared -IShared -ISource/Console \
		Source/MACLib/*.cpp Source/MACLib/Old/*.cpp \
		Source/Shared/BufferIO.cpp Source/Shared/CharacterHelper.cpp \
		Source/Shared/CircleBuffer.cpp Source/Shared/CPUFeatures.cpp \
		Source/Shared/CRC.cpp Source/Shared/GlobalFunctions.cpp \
		Source/Shared/MemoryIO.cpp Source/Shared/Semaphore.cpp \
		Source/Shared/Thread.cpp Source/Shared/WholeFileIO.cpp $(APE_TOOLS_IO_SRC) \
		Source/Console/Console.cpp -o mac$(APE_TOOLS_EXE) -lpthread ); \
	cp testdata/tools/ape-build/mac$(APE_TOOLS_EXE) $(APE_TOOLS_DIR)/; \
	rm -rf testdata/tools/ape-build; \
	printf '%s\n' "$$stamp" > $(APE_TOOLS_DIR)/.stamp; \
	echo "built $(APE_TOOLS_DIR)/mac$(APE_TOOLS_EXE)"
ape-tools-stamp:
	@$(call tool_stamp,$(APE_TOOLS_INPUTS))

# Build the Musepack reference tools from the pinned source tarballs: mpcenc
# (SV8) and mppenc 1.16 (SV7) as the only encoders the format has, mpcdec,
# mpc2sv8 (the lossless SV7-to-SV8 repacker), mpccut (the only writer of a
# nonzero beginning silence), mpcchap (the only writer of chapter packets;
# its cue-sheet path links against a libcuefile the tarball does not ship,
# so scripts/mpcchap stubs that API and the tool serves .ini chapter files
# only), and scripts/mpcdecraw, our dumper of libmpcdec's float output.
# ffmpeg has no Musepack encoder and no distribution packages these. Tests
# self-skip until this has run and WAXFLOW_REQUIRE_MPC=1 escalates. Built
# with $(CC) directly: the trees' cmake files predate CMake 4 and the
# cmake-built mppenc crashes on every encode. -fcommon covers the tentative
# definitions GCC 10 stopped merging, -fpermissive the pointer-type
# mismatches GCC 14 made errors. The mppenc tarball is bzip2, which the
# fetcher unpacks itself. On Windows (MSYS2) each tool is attempted and a
# failure leaves the rest to build; tests that need a missing tool skip.
MPC_TOOLS_VERSION := musepack-r475
MPPENC_VERSION := mppenc-1.16
# The r475 tarball unpacks to musepack_src_r475, and mppenc-1.16 to itself.
MPC_SRC := musepack_src_$(MPC_TOOLS_VERSION:musepack-%=%)
MPC_TOOLS_DIR := testdata/tools/$(MPC_TOOLS_VERSION)
MPC_TOOL_NAMES := mpcdec mpcdecraw mpcenc mpc2sv8 mpccut mpcchap
MPPENC_DIR := testdata/tools/$(MPPENC_VERSION)
MPC_BUILD := testdata/tools/mpc-build
MPC_CFLAGS := -O2 -w -fcommon -fpermissive
MPC_TOLERATE = $(if $(APE_TOOLS_WINDOWS),|| echo "skipped: did not build on this host",)
# Besides the tarballs, the build compiles in scripts/mpcdecraw and the whole
# of scripts/mpcchap.
MPC_TOOLS_INPUTS = testdata/vectors/musepack/$(MPC_SRC).tar.gz testdata/vectors/musepack/$(MPPENC_VERSION).tar.bz2 \
	scripts/mpcdecraw/mpcdecraw.c $(sort $(shell find scripts/mpcchap -type f))
mpc-tools:
	@set -e; \
	go run ./internal/testutil/cmd/vectorfetch musepack/$(MPC_SRC).tar.gz musepack/$(MPPENC_VERSION).tar.bz2; \
	stamp="$$($(call tool_stamp,$(MPC_TOOLS_INPUTS)))"; \
	built=1; for t in $(MPC_TOOL_NAMES); do \
		[ -x "$(MPC_TOOLS_DIR)/$$t$(APE_TOOLS_EXE)" ] || built=0; done; \
	[ -x "$(MPPENC_DIR)/mppenc$(APE_TOOLS_EXE)" ] || built=0; \
	[ "$$(cat $(MPC_TOOLS_DIR)/.stamp 2>/dev/null)" = "$$stamp" ] || built=0; \
	if [ "$$built" = 1 ]; then \
		echo "$(MPC_TOOLS_DIR) and $(MPPENC_DIR) are already built"; exit 0; \
	fi; \
	rm -rf $(MPC_BUILD) $(MPC_TOOLS_DIR) $(MPPENC_DIR); \
	mkdir -p $(MPC_BUILD)/out $(MPC_TOOLS_DIR) $(MPPENC_DIR); \
	go run ./internal/testutil/cmd/vectorfetch -extract $(MPC_BUILD) musepack/$(MPC_SRC).tar.gz musepack/$(MPPENC_VERSION).tar.bz2; \
	cp scripts/mpcdecraw/mpcdecraw.c $(MPC_BUILD)/$(MPC_SRC)/; \
	cp -r scripts/mpcchap $(MPC_BUILD)/$(MPC_SRC)/cuestub; \
	( cd $(MPC_BUILD)/$(MPC_SRC) && \
		$(CC) $(MPC_CFLAGS) -Iinclude -Ilibwavformat libmpcdec/*.c common/crc32.c libwavformat/output.c mpcdec/mpcdec.c -o ../out/mpcdec$(APE_TOOLS_EXE) -lm && \
		$(CC) $(MPC_CFLAGS) -Iinclude -Ilibmpcdec libmpcdec/*.c common/crc32.c mpcdecraw.c -o ../out/mpcdecraw$(APE_TOOLS_EXE) -lm && \
		( $(CC) $(MPC_CFLAGS) -DFAST_MATH -DCVD_FASTLOG -Iinclude -Ilibmpcpsy -Ilibmpcenc libmpcpsy/*.c libmpcenc/*.c common/crc32.c common/fastmath.c common/tags.c mpcenc/*.c -o ../out/mpcenc$(APE_TOOLS_EXE) -lm $(MPC_TOLERATE) ) && \
		( $(CC) $(MPC_CFLAGS) -DFAST_MATH -DCVD_FASTLOG -Iinclude -Ilibmpcdec -Ilibmpcenc libmpcdec/*.c libmpcenc/*.c common/crc32.c mpc2sv8/mpc2sv8.c -o ../out/mpc2sv8$(APE_TOOLS_EXE) -lm $(MPC_TOLERATE) ) && \
		( $(CC) $(MPC_CFLAGS) -DFAST_MATH -DCVD_FASTLOG -Iinclude -Ilibmpcdec -Ilibmpcenc libmpcdec/*.c libmpcenc/*.c common/crc32.c mpccut/mpccut.c -o ../out/mpccut$(APE_TOOLS_EXE) -lm $(MPC_TOLERATE) ) && \
		( $(CC) $(MPC_CFLAGS) -DFAST_MATH -DCVD_FASTLOG -Iinclude -Ilibmpcdec -Ilibmpcenc -Icuestub libmpcdec/*.c libmpcenc/*.c common/crc32.c common/tags.c mpcchap/mpcchap.c mpcchap/iniparser.c mpcchap/dictionary.c cuestub/cuestub.c -o ../out/mpcchap$(APE_TOOLS_EXE) -lm $(MPC_TOLERATE) ) ); \
	( cd $(MPC_BUILD)/$(MPPENC_VERSION) && \
		( $(CC) $(MPC_CFLAGS) -DMPP_ENCODER -DFAST_MATH -DCVD_FASTLOG src/*.c -o ../out/mppenc$(APE_TOOLS_EXE) -lm $(MPC_TOLERATE) ) ); \
	for t in $(MPC_TOOL_NAMES); do \
		if [ -x "$(MPC_BUILD)/out/$$t$(APE_TOOLS_EXE)" ]; then cp "$(MPC_BUILD)/out/$$t$(APE_TOOLS_EXE)" $(MPC_TOOLS_DIR)/; fi; \
	done; \
	if [ -x "$(MPC_BUILD)/out/mppenc$(APE_TOOLS_EXE)" ]; then cp "$(MPC_BUILD)/out/mppenc$(APE_TOOLS_EXE)" $(MPPENC_DIR)/; fi; \
	rm -rf $(MPC_BUILD); \
	printf '%s\n' "$$stamp" > $(MPC_TOOLS_DIR)/.stamp; \
	echo "built $(MPC_TOOL_NAMES) in $(MPC_TOOLS_DIR) and $(MPPENC_DIR)/mppenc"
mpc-tools-stamp:
	@$(call tool_stamp,$(MPC_TOOLS_INPUTS))
