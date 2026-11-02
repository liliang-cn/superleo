package app

import (
	"path/filepath"
	"strings"
)

// sentImagesHeading opens the block a sent message lists its pictures under.
// The desktop's useAttachments.build writes it, and its splitSent reads it
// back to show the pictures in the bubble; keep the three in step.
const sentImagesHeading = "[图片附件已随消息发送给视觉模型，可直接查看]"

// withSentImages is the message as the conversation keeps it: the pictures
// that went with it listed ahead of the words, the way the desktop sends them.
// A phone or any other client sends only the words and the paths, and its
// pictures went to the model but never showed in the conversation. A message
// that already lists them is left alone. Paths inside the workspace are
// written relative to it, which is how the bubble reads them back.
func withSentImages(message string, imagePaths []string, workspace string) string {
	if len(imagePaths) == 0 || strings.Contains(message, sentImagesHeading) {
		return message
	}
	var b strings.Builder
	b.WriteString(sentImagesHeading)
	for _, p := range imagePaths {
		b.WriteString("\n- ")
		b.WriteString(workspaceRelative(p, workspace))
	}
	if words := strings.TrimSpace(message); words != "" {
		b.WriteString("\n\n")
		b.WriteString(words)
	}
	return b.String()
}

// workspaceRelative is p relative to the workspace when it lies inside it,
// and p unchanged otherwise.
func workspaceRelative(p, workspace string) string {
	if workspace == "" || !filepath.IsAbs(p) {
		return p
	}
	rel, err := filepath.Rel(workspace, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	return filepath.ToSlash(rel)
}
