package voicecopy

import (
	"testing"
)

func TestBuildCreditAnnouncement(t *testing.T) {
	// Without description
	p1 := BuildCreditAnnouncement(500000, "Thanh toan tien nha", false)
	if p1 != "Đa tạ quý khách vì năm trăm nghìn đồng." {
		t.Errorf("unexpected phrase without desc: %q", p1)
	}

	// With description
	p2 := BuildCreditAnnouncement(500000, "Thanh toan tien nha", true)
	if p2 != "Đa tạ quý khách vì năm trăm nghìn đồng. Nội dung: Thanh toan tien nha." {
		t.Errorf("unexpected phrase with desc: %q", p2)
	}

	// With URL in description (sanitized out)
	p3 := BuildCreditAnnouncement(200000, "Nap tien https://scam.site/pay ngay", true)
	if p3 != "Đa tạ quý khách vì hai trăm nghìn đồng. Nội dung: Nap tien ngay." {
		t.Errorf("unexpected sanitized phrase: %q", p3)
	}
}

func TestFormatAnnouncementTemplate(t *testing.T) {
	// Custom template with {amount}
	p1 := FormatAnnouncementTemplate("Cảm ơn quý khách đã gửi {amount}.", 500000, "", false)
	if p1 != "Cảm ơn quý khách đã gửi năm trăm nghìn đồng." {
		t.Errorf("unexpected custom template: %q", p1)
	}

	// Custom template with {amount_raw}
	p2 := FormatAnnouncementTemplate("Đã nhận {amount_raw} VND.", 500000, "", false)
	if p2 != "Đã nhận 500000 VND." {
		t.Errorf("unexpected raw amount template: %q", p2)
	}

	// Custom template with {description} present
	p3 := FormatAnnouncementTemplate("Thanh toán từ {description} số tiền {amount}.", 500000, "Nguyen Van A", true)
	if p3 != "Thanh toán từ Nguyen Van A số tiền năm trăm nghìn đồng." {
		t.Errorf("unexpected desc template: %q", p3)
	}

	// Custom template with {description} when includeDescription is false
	p4 := FormatAnnouncementTemplate("Đã nhận {amount}. Nội dung: {description}.", 500000, "Secret", false)
	if p4 != "Đã nhận năm trăm nghìn đồng." {
		t.Errorf("unexpected stripped desc template: %q", p4)
	}

	// Empty template falls back to default
	p5 := FormatAnnouncementTemplate("   ", 500000, "", false)
	if p5 != "Đa tạ quý khách vì năm trăm nghìn đồng." {
		t.Errorf("unexpected fallback template: %q", p5)
	}
}

func TestBuildBurstAnnouncement(t *testing.T) {
	p := BuildBurstAnnouncement(3, 1200000)
	expected := "Bạn vừa nhận được 3 giao dịch mới, tổng cộng một triệu hai trăm nghìn đồng."
	if p != expected {
		t.Errorf("unexpected burst phrase: %q; expected %q", p, expected)
	}
}
