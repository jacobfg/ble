package att

import (
	"encoding/binary"
	"log"

	"github.com/currantlabs/ble"
)

// A DB is a contiguous range of attributes.
type DB struct {
	attrs []*attr
	base  uint16 // handle for first attr in attrs
}

const (
	tooSmall = -1
	tooLarge = -2
)

// idx returns the idx into attrs corresponding to attr a.
// If h is too small, idx returns tooSmall (-1).
// If h is too large, idx returns tooLarge (-2).
func (r *DB) idx(h int) int {
	if h < int(r.base) {
		return tooSmall
	}
	if int(h) >= int(r.base)+len(r.attrs) {
		return tooLarge
	}
	return h - int(r.base)
}

// at returns attr a.
func (r *DB) at(h uint16) (a *attr, ok bool) {
	i := r.idx(int(h))
	if i < 0 {
		return nil, false
	}
	return r.attrs[i], true
}

// subrange returns attributes in range [start, end]; it may return an empty slice.
// subrange does not panic for out-of-range start or end.
func (r *DB) subrange(start, end uint16) []*attr {
	startidx := r.idx(int(start))
	switch startidx {
	case tooSmall:
		startidx = 0
	case tooLarge:
		return []*attr{}
	}

	endidx := r.idx(int(end) + 1) // [start, end] includes its upper bound!
	switch endidx {
	case tooSmall:
		return []*attr{}
	case tooLarge:
		endidx = len(r.attrs)
	}
	return r.attrs[startidx:endidx]
}

// NewDB ...
func NewDB(ss []*ble.Service, base uint16) *DB {
	h := base
	var attrs []*attr
	var aa []*attr
	for i, s := range ss {
		h, aa = genSvcAttr(s, h)
		if i == len(ss)-1 {
			aa[0].endh = 0xFFFF
		}
		attrs = append(attrs, aa...)
	}
	DumpAttributes(attrs)
	return &DB{attrs: attrs, base: base}
}

func genSvcAttr(s *ble.Service, h uint16) (uint16, []*attr) {
	a := &attr{
		h:   h,
		typ: ble.PrimaryServiceUUID,
		v:   s.UUID,
	}
	h++
	attrs := []*attr{a}
	var aa []*attr

	for _, c := range s.Characteristics {
		h, aa = genCharAttr(c, h)
		attrs = append(attrs, aa...)
	}

	a.endh = h - 1
	return h, attrs
}

func genCharAttr(c *ble.Characteristic, h uint16) (uint16, []*attr) {
	vh := h + 1

	a := &attr{
		h:   h,
		typ: ble.CharacteristicUUID,
		v:   append([]byte{byte(c.Property), byte(vh), byte((vh) >> 8)}, c.UUID...),
	}

	va := &attr{
		h:   vh,
		typ: c.UUID,
		v:   c.Value,
		rh:  c.ReadHandler,
		wh:  c.WriteHandler,
	}

	c.Handle = h
	c.ValueHandle = vh
	if c.NotifyHandler != nil || c.IndicateHandler != nil {
		c.CCCD = newCCCD(c)
		c.Descriptors = append(c.Descriptors, c.CCCD)
	}

	h += 2

	attrs := []*attr{a, va}
	for _, d := range c.Descriptors {
		attrs = append(attrs, genDescAttr(d, h))
		h++
	}

	a.endh = h - 1
	return h, attrs
}

func genDescAttr(d *ble.Descriptor, h uint16) *attr {
	return &attr{
		h:   h,
		typ: d.UUID,
		v:   d.Value,
		rh:  d.ReadHandler,
		wh:  d.WriteHandler,
	}
}

// DumpAttributes ...
func DumpAttributes(aa []*attr) {
	log.Printf("Generating attribute table:")
	log.Printf("handle\tend\ttype\tvalue")
	for _, a := range aa {
		if a.v != nil {
			log.Printf("0x%04X\t0x%04X\t0x%s\t[ % X ]", a.h, a.endh, a.typ, a.v)
			continue
		}
		log.Printf("0x%04X\t0x%04X\t0x%s", a.h, a.endh, a.typ)
	}
}

const (
	cccNotify   = 0x0001
	cccIndicate = 0x0002
)

func newCCCD(c *ble.Characteristic) *ble.Descriptor {
	var nn ble.Notifier
	var in ble.Notifier

	d := ble.NewDescriptor(ble.ClientCharacteristicConfigUUID)

	d.HandleRead(ble.ReadHandlerFunc(func(req ble.Request, rsp ble.ResponseWriter) {
		cccs := req.Conn().Context().Value("ccc").(map[uint16]uint16)
		ccc := cccs[c.Handle]
		binary.Write(rsp, binary.LittleEndian, ccc)
	}))

	d.HandleWrite(ble.WriteHandlerFunc(func(req ble.Request, rsp ble.ResponseWriter) {
		cccs := req.Conn().Context().Value("ccc").(map[uint16]uint16)
		svr := req.Conn().Context().Value("svr").(*Server)
		ccc := cccs[c.Handle]

		newCCC := binary.LittleEndian.Uint16(req.Data())
		if newCCC&cccNotify != 0 && ccc&cccNotify == 0 {
			if c.Property&ble.CharNotify == 0 {
				rsp.SetStatus(ble.ErrUnlikely)
				return
			}
			send := func(b []byte) (int, error) { return svr.notify(c.ValueHandle, b) }
			nn = ble.NewNotifier(send)
			go c.NotifyHandler.ServeNotify(req, nn)
		}
		if newCCC&cccNotify == 0 && ccc&cccNotify != 0 {
			nn.Close()
		}
		if newCCC&cccIndicate != 0 && ccc&cccIndicate == 0 {
			if c.Property&ble.CharIndicate == 0 {
				rsp.SetStatus(ble.ErrUnlikely)
				return
			}
			send := func(b []byte) (int, error) { return svr.indicate(c.ValueHandle, b) }
			in = ble.NewNotifier(send)
			go c.IndicateHandler.ServeNotify(req, in)
		}
		if newCCC&cccIndicate == 0 && ccc&cccIndicate != 0 {
			in.Close()
		}
		cccs[c.Handle] = newCCC
	}))
	return d
}