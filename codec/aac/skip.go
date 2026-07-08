package aac

// skipDSE skips a data_stream_element (ISO 14496-3 4.4.2.7).
func skipDSE(r *bitReader) {
	r.read(4) // element_instance_tag
	align := r.bit()
	count := int(r.read(8))
	if count == 255 {
		count += int(r.read(8))
	}
	if align != 0 {
		r.byteAlign()
	}
	r.skip(count * 8)
}

// skipFIL skips a fill_element and its extension payload (4.4.2.9).
func skipFIL(r *bitReader) {
	count := int(r.read(4))
	if count == 15 {
		count += int(r.read(8)) - 1
	}
	r.skip(count * 8)
}

// skipPCE parses a program_config_element only far enough to advance the
// cursor past it (4.4.1.1). We honor the ASC channel configuration for the
// codec format, so the PCE's routing is not applied.
func skipPCE(r *bitReader) {
	r.read(4) // element_instance_tag
	r.read(2) // object_type
	r.read(4) // sampling_frequency_index
	numFront := int(r.read(4))
	numSide := int(r.read(4))
	numBack := int(r.read(4))
	numLFE := int(r.read(2))
	numAssoc := int(r.read(3))
	numCC := int(r.read(4))
	if r.bit() != 0 {
		r.read(4) // mono_mixdown_element_number
	}
	if r.bit() != 0 {
		r.read(4) // stereo_mixdown_element_number
	}
	if r.bit() != 0 {
		r.read(3) // matrix_mixdown_idx + pseudo_surround_enable
	}
	for i := 0; i < numFront+numSide+numBack; i++ {
		r.read(5) // element_is_cpe(1) + element_tag_select(4)
	}
	for i := 0; i < numLFE+numAssoc; i++ {
		r.read(4) // element_tag_select
	}
	for i := 0; i < numCC; i++ {
		r.read(5) // cc_element_is_ind_sw(1) + element_tag_select(4)
	}
	r.byteAlign()
	comment := int(r.read(8))
	r.skip(comment * 8)
}
