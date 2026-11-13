// Command winicon writes the Windows icon resource that the Go linker embeds
// in token-usage.exe: one COFF object (.syso) per architecture, holding the
// images of an .ico file as RT_ICON resources and one RT_GROUP_ICON. Explorer,
// the taskbar and Installed apps show that icon.
//
//	go run ./scripts/winicon -ico cmd/token-usage/winres/icon.ico -out cmd/token-usage
package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/png"
	"log"
	"os"
	"path/filepath"
)

const (
	rtIcon      = 3
	rtGroupIcon = 14
	langEnUS    = 0x0409
)

type iconImage struct {
	width, height int
	bitCount      uint16
	data          []byte
}

func main() {
	icoPath := flag.String("ico", "", "multi-size .ico file")
	outDir := flag.String("out", ".", "folder for rsrc_windows_<arch>.syso")
	flag.Parse()
	if *icoPath == "" {
		log.Fatal("-ico is required")
	}
	images, err := readICO(*icoPath)
	if err != nil {
		log.Fatal(err)
	}
	section, relocations := resourceSection(images)
	for _, target := range []struct {
		arch    string
		machine uint16
		relType uint16
	}{
		{"amd64", 0x8664, 3}, // IMAGE_REL_AMD64_ADDR32NB
		{"arm64", 0xAA64, 2}, // IMAGE_REL_ARM64_ADDR32NB
		{"386", 0x014C, 7},   // IMAGE_REL_I386_DIR32NB
	} {
		path := filepath.Join(*outDir, "rsrc_windows_"+target.arch+".syso")
		if err := os.WriteFile(path, coffObject(target.machine, target.relType, section, relocations), 0o644); err != nil {
			log.Fatal(err)
		}
		fmt.Println("wrote", path)
	}
}

// readICO returns the images of an .ico file. PNG images smaller than 256
// pixels become 32-bit DIBs, the form every Windows icon loader accepts.
func readICO(path string) ([]iconImage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 6 || binary.LittleEndian.Uint16(data[2:]) != 1 {
		return nil, errors.New("not an .ico file")
	}
	count := int(binary.LittleEndian.Uint16(data[4:]))
	var images []iconImage
	for index := 0; index < count; index++ {
		entry := data[6+16*index:]
		size := int(binary.LittleEndian.Uint32(entry[8:]))
		offset := int(binary.LittleEndian.Uint32(entry[12:]))
		if offset+size > len(data) {
			return nil, fmt.Errorf("image %d is truncated", index)
		}
		raw := data[offset : offset+size]
		width, height := int(entry[0]), int(entry[1])
		if width == 0 {
			width = 256
		}
		if height == 0 {
			height = 256
		}
		image := iconImage{width: width, height: height, bitCount: 32, data: raw}
		if bytes.HasPrefix(raw, []byte("\x89PNG")) && width < 256 {
			if image.data, err = pngToDIB(raw); err != nil {
				return nil, fmt.Errorf("image %d: %w", index, err)
			}
		}
		images = append(images, image)
	}
	return images, nil
}

// pngToDIB converts a PNG into an icon DIB: a BITMAPINFOHEADER with doubled
// height, bottom-up BGRA rows, then the 1-bit AND mask.
func pngToDIB(data []byte) ([]byte, error) {
	decoded, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	bounds := decoded.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	maskStride := ((width + 31) / 32) * 4
	var out bytes.Buffer
	header := []any{
		uint32(40), int32(width), int32(height * 2), uint16(1), uint16(32), uint32(0),
		uint32(width*height*4 + maskStride*height), int32(0), int32(0), uint32(0), uint32(0),
	}
	for _, field := range header {
		_ = binary.Write(&out, binary.LittleEndian, field)
	}
	nrgba := image.NewNRGBA(bounds)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			nrgba.Set(x, y, decoded.At(x, y))
		}
	}
	for y := height - 1; y >= 0; y-- {
		for x := 0; x < width; x++ {
			pixel := nrgba.NRGBAAt(bounds.Min.X+x, bounds.Min.Y+y)
			out.Write([]byte{pixel.B, pixel.G, pixel.R, pixel.A})
		}
	}
	for y := height - 1; y >= 0; y-- {
		row := make([]byte, maskStride)
		for x := 0; x < width; x++ {
			if nrgba.NRGBAAt(bounds.Min.X+x, bounds.Min.Y+y).A == 0 {
				row[x/8] |= 0x80 >> (x % 8)
			}
		}
		out.Write(row)
	}
	return out.Bytes(), nil
}

type leaf struct {
	typeID, nameID uint32
	data           []byte
}

// resourceSection lays out the .rsrc tree (type, name, language, data entry)
// and returns it with the offsets of the data-entry RVA fields, which the
// linker must relocate.
func resourceSection(images []iconImage) ([]byte, []uint32) {
	var group bytes.Buffer
	_ = binary.Write(&group, binary.LittleEndian, []uint16{0, 1, uint16(len(images))})
	var leaves []leaf
	for index, image := range images {
		width, height := byte(image.width), byte(image.height)
		if image.width >= 256 {
			width = 0
		}
		if image.height >= 256 {
			height = 0
		}
		group.Write([]byte{width, height, 0, 0})
		_ = binary.Write(&group, binary.LittleEndian, uint16(1))
		_ = binary.Write(&group, binary.LittleEndian, image.bitCount)
		_ = binary.Write(&group, binary.LittleEndian, uint32(len(image.data)))
		_ = binary.Write(&group, binary.LittleEndian, uint16(index+1))
		leaves = append(leaves, leaf{rtIcon, uint32(index + 1), image.data})
	}
	leaves = append(leaves, leaf{rtGroupIcon, 1, group.Bytes()})

	types := []uint32{rtIcon, rtGroupIcon}
	namesOf := func(typeID uint32) []leaf {
		var out []leaf
		for _, l := range leaves {
			if l.typeID == typeID {
				out = append(out, l)
			}
		}
		return out
	}
	dirSize := func(entries int) int { return 16 + 8*entries }

	// Offsets: root, then one name directory per type, then one language
	// directory per leaf, then data entries, then the data.
	offset := dirSize(len(types))
	nameDir := map[uint32]int{}
	for _, t := range types {
		nameDir[t] = offset
		offset += dirSize(len(namesOf(t)))
	}
	langDir := make([]int, len(leaves))
	for i := range leaves {
		langDir[i] = offset
		offset += dirSize(1)
	}
	dataEntry := make([]int, len(leaves))
	for i := range leaves {
		dataEntry[i] = offset
		offset += 16
	}
	dataAt := make([]int, len(leaves))
	for i, l := range leaves {
		offset = (offset + 7) &^ 7
		dataAt[i] = offset
		offset += len(l.data)
	}
	section := make([]byte, (offset+7)&^7)
	le := binary.LittleEndian
	writeDir := func(at int, entries [][2]uint32) {
		le.PutUint16(section[at+14:], uint16(len(entries)))
		for i, e := range entries {
			le.PutUint32(section[at+16+8*i:], e[0])
			le.PutUint32(section[at+20+8*i:], e[1])
		}
	}
	const subdirectory = 0x80000000
	var rootEntries [][2]uint32
	for _, t := range types {
		rootEntries = append(rootEntries, [2]uint32{t, subdirectory | uint32(nameDir[t])})
	}
	writeDir(0, rootEntries)
	for _, t := range types {
		var entries [][2]uint32
		for i, l := range leaves {
			if l.typeID == t {
				entries = append(entries, [2]uint32{l.nameID, subdirectory | uint32(langDir[i])})
			}
		}
		writeDir(nameDir[t], entries)
	}
	var relocations []uint32
	for i, l := range leaves {
		writeDir(langDir[i], [][2]uint32{{langEnUS, uint32(dataEntry[i])}})
		le.PutUint32(section[dataEntry[i]:], uint32(dataAt[i])) // section offset; relocated to an RVA
		le.PutUint32(section[dataEntry[i]+4:], uint32(len(l.data)))
		relocations = append(relocations, uint32(dataEntry[i]))
		copy(section[dataAt[i]:], l.data)
	}
	return section, relocations
}

// coffObject wraps the section in a COFF object with one section symbol that
// every relocation refers to.
func coffObject(machine, relType uint16, section []byte, relocations []uint32) []byte {
	const fileHeader, sectionHeader, relocationSize = 20, 40, 10
	rawAt := fileHeader + sectionHeader
	relAt := rawAt + len(section)
	symAt := relAt + relocationSize*len(relocations)
	var out bytes.Buffer
	le := binary.LittleEndian
	_ = binary.Write(&out, le, machine)
	_ = binary.Write(&out, le, uint16(1)) // sections
	_ = binary.Write(&out, le, uint32(0)) // timestamp
	_ = binary.Write(&out, le, uint32(symAt))
	_ = binary.Write(&out, le, uint32(1)) // symbols
	_ = binary.Write(&out, le, uint16(0)) // optional header
	_ = binary.Write(&out, le, uint16(0)) // characteristics
	out.Write([]byte(".rsrc\x00\x00\x00"))
	_ = binary.Write(&out, le, []uint32{0, 0, uint32(len(section)), uint32(rawAt), uint32(relAt), 0})
	_ = binary.Write(&out, le, uint16(len(relocations)))
	_ = binary.Write(&out, le, uint16(0))
	_ = binary.Write(&out, le, uint32(0x40000040)) // initialized data, readable
	out.Write(section)
	for _, at := range relocations {
		_ = binary.Write(&out, le, at)
		_ = binary.Write(&out, le, uint32(0)) // symbol 0: the section
		_ = binary.Write(&out, le, relType)
	}
	out.Write([]byte(".rsrc\x00\x00\x00"))
	_ = binary.Write(&out, le, uint32(0)) // value
	_ = binary.Write(&out, le, int16(1))  // section number
	_ = binary.Write(&out, le, uint16(0)) // type
	out.Write([]byte{3, 0})               // static, no aux symbols
	_ = binary.Write(&out, le, uint32(4)) // empty string table
	return out.Bytes()
}
