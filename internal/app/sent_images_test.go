package app

import "testing"

func TestWithSentImages(t *testing.T) {
	ws := "/home/me/workspace"
	for _, c := range []struct {
		name, msg string
		images    []string
		want      string
	}{
		{"no pictures", "hello", nil, "hello"},
		{"phone picture inside the workspace", "招商永隆的", []string{ws + "/uploads/phone-1.jpg"},
			sentImagesHeading + "\n- uploads/phone-1.jpg\n\n招商永隆的"},
		{"picture outside the workspace", "看看", []string{"/tmp/x.png"},
			sentImagesHeading + "\n- /tmp/x.png\n\n看看"},
		{"a sibling folder is not inside", "", []string{"/home/me/workspace2/a.png"},
			sentImagesHeading + "\n- /home/me/workspace2/a.png"},
		{"desktop already listed them", sentImagesHeading + "\n- uploads/a.png\n\nhi", []string{"uploads/a.png"},
			sentImagesHeading + "\n- uploads/a.png\n\nhi"},
	} {
		if got := withSentImages(c.msg, c.images, ws); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}
