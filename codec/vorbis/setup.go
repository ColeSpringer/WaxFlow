package vorbis

// mode is one Vorbis mode (spec 4.2.4 step 6): a block-size flag and the
// mapping it uses. window/transform types are always 0 in Vorbis I.
type mode struct {
	blockflag bool
	mapping   int
}

// submap pairs a floor and a residue; a channel is routed to one submap.
type submap struct {
	floor   int
	residue int
}

// coupling is one channel-coupling step: a magnitude and an angle channel.
type mapping struct {
	submaps     []submap
	couplingMag []int
	couplingAng []int
	mux         []int // per-channel submap index
}

// parseSetup reads the entire setup header (packet type 5) into the Config.
// The identification header must already be parsed (channels/blocksizes set).
func (c *Config) parseSetup(pkt []byte) error {
	// The 7-byte header: type byte 0x05 then "vorbis".
	if len(pkt) < 7 || pkt[0] != 0x05 || string(pkt[1:7]) != "vorbis" {
		return malformed("setup header lacks the type/signature")
	}
	r := newBitReader(pkt[7:])

	// Codebooks.
	numCodebooks := int(r.read(8)) + 1
	c.codebooks = make([]codebook, numCodebooks)
	for i := range c.codebooks {
		cb, err := parseCodebook(r)
		if err != nil {
			return err
		}
		c.codebooks[i] = cb
	}

	// Time-domain transforms: placeholder, every value must be zero.
	numTimes := int(r.read(6)) + 1
	for i := 0; i < numTimes; i++ {
		if v := r.read(16); v != 0 {
			return malformed("nonzero time-domain transform %d", v)
		}
	}

	// Floors.
	numFloors := int(r.read(6)) + 1
	if numFloors > maxFloors {
		return malformed("%d floors (max %d)", numFloors, maxFloors)
	}
	c.floors = make([]floor, numFloors)
	for i := range c.floors {
		typ := r.read(16)
		var (
			f   floor
			err error
		)
		switch typ {
		case 0:
			f, err = parseFloor0(r, c.codebooks)
		case 1:
			f, err = parseFloor1(r, len(c.codebooks))
		default:
			return malformed("floor type %d", typ)
		}
		if err != nil {
			return err
		}
		c.floors[i] = f
	}

	// Residues.
	numResidues := int(r.read(6)) + 1
	if numResidues > maxResidues {
		return malformed("%d residues (max %d)", numResidues, maxResidues)
	}
	c.residues = make([]residue, numResidues)
	for i := range c.residues {
		res, err := parseResidue(r, len(c.codebooks))
		if err != nil {
			return err
		}
		c.residues[i] = res
	}

	// Mappings.
	numMappings := int(r.read(6)) + 1
	if numMappings > maxMappings {
		return malformed("%d mappings (max %d)", numMappings, maxMappings)
	}
	c.mappings = make([]mapping, numMappings)
	for i := range c.mappings {
		if typ := r.read(16); typ != 0 {
			return malformed("mapping type %d", typ)
		}
		m, err := c.parseMapping(r, numFloors, numResidues)
		if err != nil {
			return err
		}
		c.mappings[i] = m
	}

	// Modes.
	numModes := int(r.read(6)) + 1
	if numModes > maxModes {
		return malformed("%d modes (max %d)", numModes, maxModes)
	}
	c.modes = make([]mode, numModes)
	for i := range c.modes {
		c.modes[i].blockflag = r.bit() == 1
		if w := r.read(16); w != 0 {
			return malformed("mode window type %d", w)
		}
		if tr := r.read(16); tr != 0 {
			return malformed("mode transform type %d", tr)
		}
		m := int(r.read(8))
		if m >= numMappings {
			return malformed("mode references mapping %d of %d", m, numMappings)
		}
		c.modes[i].mapping = m
	}

	if r.bit() != 1 {
		return malformed("setup header framing bit not set")
	}
	if r.eof {
		return malformed("setup header truncated")
	}
	return nil
}

func (c *Config) parseMapping(r *bitReader, numFloors, numResidues int) (mapping, error) {
	var m mapping
	submaps := 1
	if r.bit() == 1 {
		submaps = int(r.read(4)) + 1
	}
	if submaps > maxSubmaps {
		return m, malformed("%d submaps (max %d)", submaps, maxSubmaps)
	}
	if r.bit() == 1 {
		steps := int(r.read(8)) + 1
		magBits := ilog(c.Channels - 1)
		for i := 0; i < steps; i++ {
			mag := int(r.read(magBits))
			ang := int(r.read(magBits))
			if mag == ang || mag >= c.Channels || ang >= c.Channels {
				return m, malformed("invalid coupling step %d/%d", mag, ang)
			}
			m.couplingMag = append(m.couplingMag, mag)
			m.couplingAng = append(m.couplingAng, ang)
		}
	}
	if v := r.read(2); v != 0 {
		return m, malformed("nonzero mapping reserved field %d", v)
	}
	m.mux = make([]int, c.Channels)
	if submaps > 1 {
		for ch := 0; ch < c.Channels; ch++ {
			m.mux[ch] = int(r.read(4))
			if m.mux[ch] >= submaps {
				return m, malformed("channel %d muxes to submap %d of %d", ch, m.mux[ch], submaps)
			}
		}
	}
	m.submaps = make([]submap, submaps)
	for i := 0; i < submaps; i++ {
		r.read(8) // unused time-config placeholder
		fl := int(r.read(8))
		if fl >= numFloors {
			return m, malformed("submap floor %d of %d", fl, numFloors)
		}
		res := int(r.read(8))
		if res >= numResidues {
			return m, malformed("submap residue %d of %d", res, numResidues)
		}
		m.submaps[i] = submap{floor: fl, residue: res}
	}
	if r.eof {
		return m, malformed("mapping truncated")
	}
	return m, nil
}

// ParseHeaders parses the three Vorbis header packets (identification,
// comment, setup) into a Config. The comment packet is validated for type and
// signature but its content is left to the container's metadata mapper.
func ParseHeaders(id, comment, setup []byte) (Config, error) {
	var c Config
	if err := c.parseID(id); err != nil {
		return c, err
	}
	if len(comment) < 7 || comment[0] != 0x03 || string(comment[1:7]) != "vorbis" {
		return c, malformed("comment header lacks the type/signature")
	}
	if err := c.parseSetup(setup); err != nil {
		return c, err
	}
	return c, nil
}

// parseID parses the identification header (packet type 1, spec 4.2.2).
func (c *Config) parseID(pkt []byte) error {
	if len(pkt) < 7 || pkt[0] != 0x01 || string(pkt[1:7]) != "vorbis" {
		return malformed("identification header lacks the type/signature")
	}
	r := newBitReader(pkt[7:])
	c.Version = r.read(32)
	if c.Version != 0 {
		return malformed("unsupported bitstream version %d", c.Version)
	}
	c.Channels = int(r.read(8))
	if c.Channels < 1 || c.Channels > maxChannels {
		return malformed("%d channels outside 1..%d", c.Channels, maxChannels)
	}
	c.Rate = int(r.read(32))
	if c.Rate <= 0 {
		return malformed("sample rate %d", c.Rate)
	}
	r.read(32) // bitrate_maximum
	c.Bitrate = int(int32(r.read(32)))
	r.read(32) // bitrate_minimum
	bs0 := int(r.read(4))
	bs1 := int(r.read(4))
	if bs0 < minBlockLog || bs0 > maxBlockLog || bs1 < minBlockLog || bs1 > maxBlockLog || bs0 > bs1 {
		return malformed("invalid block sizes 2^%d/2^%d", bs0, bs1)
	}
	c.blockSizes[0] = 1 << bs0
	c.blockSizes[1] = 1 << bs1
	if r.bit() != 1 {
		return malformed("identification framing bit not set")
	}
	if r.eof {
		return malformed("identification header truncated")
	}
	return nil
}
