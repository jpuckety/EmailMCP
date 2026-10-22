package imap

import (
	"testing"

	goimap "github.com/emersion/go-imap/v2"
)

func TestExtractAttachments_PartIDAndInline(t *testing.T) {
	bs := &goimap.BodyStructureMultiPart{
		Subtype: "mixed",
		Children: []goimap.BodyStructure{
			&goimap.BodyStructureSinglePart{Type: "text", Subtype: "plain"},
			&goimap.BodyStructureSinglePart{
				Type: "application", Subtype: "pdf", Size: 123,
				Extended: &goimap.BodyStructureSinglePartExt{
					Disposition: &goimap.BodyStructureDisposition{
						Value:  "attachment",
						Params: map[string]string{"filename": "a.pdf"},
					},
				},
			},
			&goimap.BodyStructureSinglePart{
				Type: "image", Subtype: "png", ID: "<img1@x>", Size: 10,
				Extended: &goimap.BodyStructureSinglePartExt{
					Disposition: &goimap.BodyStructureDisposition{Value: "inline", Params: map[string]string{"filename": "x.png"}},
				},
			},
		},
	}
	atts := extractAttachments(bs)
	if len(atts) != 2 {
		t.Fatalf("got %d atts: %+v", len(atts), atts)
	}
	if atts[0].PartID != "2" || atts[0].Filename != "a.pdf" || atts[0].Size != 123 {
		t.Fatalf("att0 %+v", atts[0])
	}
	if atts[1].PartID != "3" || !atts[1].Inline || atts[1].ContentID != "img1@x" {
		t.Fatalf("att1 %+v", atts[1])
	}
	if !hasAttachmentStructure(bs) {
		t.Fatal("expected has attachments")
	}
}

func TestExtractAttachments_Base64SizeIsDecoded(t *testing.T) {
	bs := &goimap.BodyStructureMultiPart{
		Subtype: "mixed",
		Children: []goimap.BodyStructure{
			&goimap.BodyStructureSinglePart{Type: "text", Subtype: "plain"},
			&goimap.BodyStructureSinglePart{
				Type: "text", Subtype: "plain", Encoding: "base64", Size: 40,
				Extended: &goimap.BodyStructureSinglePartExt{
					Disposition: &goimap.BodyStructureDisposition{
						Value:  "attachment",
						Params: map[string]string{"filename": "note.txt"},
					},
				},
			},
		},
	}
	atts := extractAttachments(bs)
	if len(atts) != 1 {
		t.Fatalf("got %d atts: %+v", len(atts), atts)
	}
	if atts[0].Size != 30 {
		t.Fatalf("size=%d want 30 (decoded from 40-byte base64)", atts[0].Size)
	}
}
