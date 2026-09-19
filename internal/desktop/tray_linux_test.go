//go:build linux

package desktop

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestTrayMenuLayout(t *testing.T) {
	var clicked []string
	m := &trayMenu{items: []trayMenuItem{
		{label: "Open", action: func() { clicked = append(clicked, "open") }},
		{},
		{label: "Quit", action: func() { clicked = append(clicked, "quit") }},
	}}

	rev, root, derr := m.GetLayout(0, -1, nil)
	if derr != nil {
		t.Fatalf("GetLayout(0): %v", derr)
	}
	if rev != 1 || root.ID != 0 || len(root.Children) != 3 {
		t.Fatalf("root = rev %d id %d with %d children, want rev 1 id 0 with 3", rev, root.ID, len(root.Children))
	}
	tests := []struct {
		child int
		label string
		sep   bool
	}{
		{0, "Open", false},
		{1, "", true},
		{2, "Quit", false},
	}
	for _, tt := range tests {
		l := root.Children[tt.child].Value().(menuLayout)
		if l.ID != int32(tt.child+1) {
			t.Errorf("child %d id = %d, want %d", tt.child, l.ID, tt.child+1)
		}
		label, _ := l.Properties["label"].Value().(string)
		kind, _ := l.Properties["type"].Value().(string)
		if label != tt.label || (kind == "separator") != tt.sep {
			t.Errorf("child %d = label %q type %q, want label %q separator %v", tt.child, label, kind, tt.label, tt.sep)
		}
	}

	if _, shallow, _ := m.GetLayout(0, 0, nil); len(shallow.Children) != 0 {
		t.Errorf("depth 0 returned %d children, want none", len(shallow.Children))
	}
	if _, _, derr := m.GetLayout(9, -1, nil); derr == nil {
		t.Error("GetLayout(9) succeeded for a missing item")
	}
	props, _ := m.GetGroupProperties([]int32{3, 9, 1}, nil)
	if len(props) != 2 || props[0].ID != 3 || props[1].ID != 1 {
		t.Errorf("GetGroupProperties skipped the wrong ids: %+v", props)
	}

	m.Event(1, "clicked", dbus.MakeVariant(0), 0)
	m.Event(2, "clicked", dbus.MakeVariant(0), 0)
	m.Event(3, "hovered", dbus.MakeVariant(0), 0)
	m.Event(9, "clicked", dbus.MakeVariant(0), 0)
	m.EventGroup([]menuEvent{{ID: 3, EventID: "clicked"}})
	if got := len(clicked); got != 2 || clicked[0] != "open" || clicked[1] != "quit" {
		t.Errorf("clicked = %v, want [open quit]", clicked)
	}
}

// buildICO packs frames into an .ico container; a frame that is not png is
// stored as-is, standing in for a Windows bitmap frame.
func buildICO(frames ...[]byte) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, []uint16{0, 1, uint16(len(frames))})
	offset := 6 + 16*len(frames)
	for _, f := range frames {
		b.Write([]byte{0, 0, 0, 0})
		binary.Write(&b, binary.LittleEndian, []uint16{1, 32})
		binary.Write(&b, binary.LittleEndian, []uint32{uint32(len(f)), uint32(offset)})
		offset += len(f)
	}
	for _, f := range frames {
		b.Write(f)
	}
	return b.Bytes()
}

func TestPixmapsFromICO(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	img.SetNRGBA(0, 0, color.NRGBA{R: 0x11, G: 0x22, B: 0x33, A: 0xff})
	img.SetNRGBA(1, 0, color.NRGBA{R: 0x40, G: 0x80, B: 0xc0, A: 0x80})
	var frame bytes.Buffer
	if err := png.Encode(&frame, img); err != nil {
		t.Fatal(err)
	}
	bitmap := []byte("BM not a png frame")

	pixmaps, err := pixmapsFromICO(buildICO(bitmap, frame.Bytes(), frame.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(pixmaps) != 2 {
		t.Fatalf("got %d pixmaps, want the 2 png frames", len(pixmaps))
	}
	pm := pixmaps[0]
	if pm.Width != 2 || pm.Height != 1 {
		t.Errorf("size = %dx%d, want 2x1", pm.Width, pm.Height)
	}
	// ARGB, big-endian, straight alpha: the half-transparent pixel keeps its
	// original channel values rather than premultiplied ones.
	want := []byte{0xff, 0x11, 0x22, 0x33, 0x80, 0x40, 0x80, 0xc0}
	if !bytes.Equal(pm.Data, want) {
		t.Errorf("data = % x, want % x", pm.Data, want)
	}

	bad := []struct {
		name string
		data []byte
	}{
		{"garbage", []byte("not an icon")},
		{"only bitmap frames", buildICO(bitmap)},
		{"frame past the end", buildICO(frame.Bytes())[:40]},
		{"corrupt png frame", buildICO(append(append([]byte{}, pngMagic...), "junk"...))},
	}
	for _, tt := range bad {
		if _, err := pixmapsFromICO(tt.data); err == nil {
			t.Errorf("%s decoded without error", tt.name)
		}
	}
}

func TestActivationTokenFollowsBusOrder(t *testing.T) {
	sni := &statusNotifier{}
	call := func(member string, body ...interface{}) *dbus.Message {
		return &dbus.Message{
			Type: dbus.TypeMethodCall,
			Headers: map[dbus.HeaderField]dbus.Variant{
				dbus.FieldInterface: dbus.MakeVariant(sniInterface),
				dbus.FieldMember:    dbus.MakeVariant(member),
			},
			Body: body,
		}
	}

	// a token for opening the context menu is never spent; the one sent for
	// the click must replace it, and a click spends exactly one token.
	sni.observe(call("ProvideXdgActivationToken", "kwin-1"))
	sni.observe(call("ProvideXdgActivationToken", "kwin-2"))
	if got := sni.takeToken(); got != "kwin-2" {
		t.Errorf("takeToken() = %q, want the newest kwin-2", got)
	}
	if got := sni.takeToken(); got != "" {
		t.Errorf("second takeToken() = %q, want the token spent", got)
	}

	// other calls, and other interfaces, must not be mistaken for a token.
	sni.observe(call("Activate", int32(1), int32(2)))
	sni.observe(&dbus.Message{Type: dbus.TypeSignal, Body: []interface{}{"kwin-3"}})
	if got := sni.takeToken(); got != "" {
		t.Errorf("takeToken() = %q after unrelated messages, want none", got)
	}
}
