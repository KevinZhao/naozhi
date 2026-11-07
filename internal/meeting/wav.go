package meeting

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// The helper writes 16 kHz mono 16-bit PCM segments; the gateway only joins them, so it reads
// just enough of RIFF to find the data chunk.
const (
	SampleRate     = 16000
	bytesPerSample = 2
)

// wavData returns the offset and length of f's PCM data. A segment cut short (helper killed)
// has a zero or oversized data length; the data then runs to the end of the file.
func wavData(f *os.File) (off, n int64, err error) {
	st, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	var hdr [12]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return 0, 0, err
	}
	if string(hdr[0:4]) != "RIFF" || string(hdr[8:12]) != "WAVE" {
		return 0, 0, errors.New("not a WAV file")
	}
	pos := int64(12)
	for {
		var ch [8]byte
		if _, err := f.ReadAt(ch[:], pos); err != nil {
			return 0, 0, fmt.Errorf("no data chunk: %w", err)
		}
		size := int64(binary.LittleEndian.Uint32(ch[4:8]))
		pos += 8
		if string(ch[0:4]) == "fmt " {
			var fm [16]byte
			if _, err := f.ReadAt(fm[:], pos); err != nil {
				return 0, 0, err
			}
			format, chans := binary.LittleEndian.Uint16(fm[0:2]), binary.LittleEndian.Uint16(fm[2:4])
			rate, bits := binary.LittleEndian.Uint32(fm[4:8]), binary.LittleEndian.Uint16(fm[14:16])
			if (format != 1 && format != 0xFFFE) || chans != 1 || rate != SampleRate || bits != 16 {
				return 0, 0, fmt.Errorf("unexpected format %d/%dch/%dHz/%dbit", format, chans, rate, bits)
			}
		}
		if string(ch[0:4]) == "data" {
			if size == 0 || pos+size > st.Size() {
				size = st.Size() - pos
			}
			return pos, size &^ 1, nil
		}
		pos += size + size&1
	}
}

func wavHeader(dataLen int64) []byte {
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+dataLen))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], 1)
	binary.LittleEndian.PutUint32(h[24:], SampleRate)
	binary.LittleEndian.PutUint32(h[28:], SampleRate*bytesPerSample)
	binary.LittleEndian.PutUint16(h[32:], bytesPerSample)
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(dataLen))
	return h
}

// ConcatWAV joins the segments into dst, in order, and returns the total seconds. A segment
// that cannot be read is replaced by silence of padSeconds[i] (when given) so the track keeps
// its alignment with the other one.
func ConcatWAV(dst string, srcs []string, padSeconds []float64) (float64, error) {
	out, err := os.OpenFile(dst+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer os.Remove(dst + ".tmp")
	if _, err := out.Write(wavHeader(0)); err != nil {
		out.Close()
		return 0, err
	}
	var total int64
	for i, src := range srcs {
		n, err := appendPCM(out, src)
		if err != nil {
			if i >= len(padSeconds) {
				out.Close()
				return 0, fmt.Errorf("%s: %w", src, err)
			}
			n, err = writeSilence(out, padSeconds[i])
			if err != nil {
				out.Close()
				return 0, err
			}
		}
		total += n
	}
	if _, err := out.WriteAt(wavHeader(total), 0); err != nil {
		out.Close()
		return 0, err
	}
	if err := out.Close(); err != nil {
		return 0, err
	}
	return float64(total) / (SampleRate * bytesPerSample), os.Rename(dst+".tmp", dst)
}

func appendPCM(out io.Writer, src string) (int64, error) {
	f, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	off, n, err := wavData(f)
	if err != nil {
		return 0, err
	}
	return io.Copy(out, io.NewSectionReader(f, off, n))
}

func writeSilence(out io.Writer, seconds float64) (int64, error) {
	n := int64(seconds*SampleRate) * bytesPerSample
	buf := make([]byte, 32*1024)
	var done int64
	for done < n {
		k := min(int64(len(buf)), n-done)
		w, err := out.Write(buf[:k])
		done += int64(w)
		if err != nil {
			return done, err
		}
	}
	return done, nil
}

// WAVSeconds is the length of a WAV file the helper or ConcatWAV wrote.
func WAVSeconds(path string) (float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	_, n, err := wavData(f)
	return float64(n) / (SampleRate * bytesPerSample), err
}
