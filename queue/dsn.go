package queue

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/mjl-/bstore"

	"github.com/mjl-/mox/dns"
	"github.com/mjl-/mox/dsn"
	"github.com/mjl-/mox/message"
	"github.com/mjl-/mox/mlog"
	"github.com/mjl-/mox/mox-"
	"github.com/mjl-/mox/smtp"
	"github.com/mjl-/mox/smtpclient"
	"github.com/mjl-/mox/store"
)

var (
	metricDMARCReportFailure = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "mox_queue_dmarcreport_failure_total",
			Help: "Permanent failures to deliver a DMARC report.",
		},
	)
)

// todo: rename function, perhaps put some of the params in a delivery struct so we don't pass all the params all the time?
func fail(ctx context.Context, qlog mlog.Log, msgs []*Msg, dialedIPs map[string][]net.IP, backoff time.Duration, remoteMTA dsn.NameIP, err error) {
	// todo future: when we implement relaying, we should be able to send DSNs to non-local users. and possibly specify a null mailfrom. ../rfc/5321:1503
	// todo future: when we implement relaying, and a dsn cannot be delivered, and requiretls was active, we cannot drop the message. instead deliver to local postmaster? though ../rfc/8689:383 may intend to say the dsn should be delivered without requiretls?
	// todo future: when we implement smtp dsn extension, parameter RET=FULL must be disregarded for messages with REQUIRETLS. ../rfc/8689:379

	m0 := msgs[0]

	var smtpLines []string
	var cerr smtpclient.Error
	var permanent bool
	var errmsg = err.Error()
	var code int
	var secodeOpt string
	if errors.As(err, &cerr) {
		if cerr.Line != "" {
			smtpLines = append([]string{cerr.Line}, cerr.MoreLines...)
		}
		permanent = cerr.Permanent
		code = cerr.Code
		secodeOpt = cerr.Secode
	}
	qlog = qlog.With(
		slog.Bool("permanent", permanent),
		slog.Int("code", code),
		slog.String("secode", secodeOpt),
	)

	ids := make([]int64, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
	}

	if permanent || m0.MaxAttempts == 0 && m0.Attempts >= 8 || m0.MaxAttempts > 0 && m0.Attempts >= m0.MaxAttempts {
		for _, m := range msgs {
			qmlog := qlog.With(slog.Int64("msgid", m.ID), slog.Any("recipient", m.Recipient()))
			qmlog.Errorx("permanent failure delivering from queue", err)
			deliverDSNFailure(ctx, qmlog, *m, remoteMTA, secodeOpt, errmsg, smtpLines)
		}
		if err := queueDelete(context.Background(), ids...); err != nil {
			qlog.Errorx("deleting messages from queue after permanent failure", err)
		}
		return
	}

	// All messages should have the same DialedIPs, so we can update them all at once.
	qup := bstore.QueryDB[Msg](context.Background(), DB)
	qup.FilterIDs(ids)
	if _, xerr := qup.UpdateNonzero(Msg{LastError: errmsg, DialedIPs: dialedIPs}); err != nil {
		qlog.Errorx("storing delivery error", xerr, slog.String("deliveryerror", errmsg))
	}

	if m0.Attempts == 5 {
		// We've attempted deliveries at these intervals: 0, 7.5m, 15m, 30m, 1h, 2u.
		// Let sender know delivery is delayed.

		retryUntil := m0.LastAttempt.Add((4 + 8 + 16) * time.Hour)
		for _, m := range msgs {
			qmlog := qlog.With(slog.Int64("msgid", m.ID), slog.Any("recipient", m.Recipient()))
			qmlog.Errorx("temporary failure delivering from queue, sending delayed dsn", err, slog.Duration("backoff", backoff))
			deliverDSNDelay(ctx, qmlog, *m, remoteMTA, secodeOpt, errmsg, smtpLines, retryUntil)
		}
	} else {
		for _, m := range msgs {
			qlog.Errorx("temporary failure delivering from queue", err,
				slog.Int64("msgid", m.ID),
				slog.Any("recipient", m.Recipient()),
				slog.Duration("backoff", backoff),
				slog.Time("nextattempt", m0.NextAttempt))
		}
	}
}

func deliverDSNFailure(ctx context.Context, log mlog.Log, m Msg, remoteMTA dsn.NameIP, secodeOpt, errmsg string, smtpLines []string) {
	const subject = "mail delivery failed"
	message := fmt.Sprintf(`
Delivery has failed permanently for your email to:

	%s

No further deliveries will be attempted.

Error during the last delivery attempt:

	%s
`, m.Recipient().XString(m.SMTPUTF8), errmsg)
	if len(smtpLines) > 0 {
		message += "\nFull SMTP response:\n\n\t" + strings.Join(smtpLines, "\n\t") + "\n"
	}

	deliverDSN(ctx, log, m, remoteMTA, secodeOpt, errmsg, smtpLines, true, nil, subject, message)
}

func deliverDSNDelay(ctx context.Context, log mlog.Log, m Msg, remoteMTA dsn.NameIP, secodeOpt, errmsg string, smtpLines []string, retryUntil time.Time) {
	// Should not happen, but doesn't hurt to prevent sending delayed delivery
	// notifications for DMARC reports. We don't want to waste postmaster attention.
	if m.IsDMARCReport {
		return
	}

	const subject = "mail delivery delayed"
	message := fmt.Sprintf(`
Delivery has been delayed of your email to:

	%s

Next attempts to deliver: in 4 hours, 8 hours and 16 hours.
If these attempts all fail, you will receive a notice.

Error during the last delivery attempt:

	%s
`, m.Recipient().XString(false), errmsg)
	if len(smtpLines) > 0 {
		message += "\nFull SMTP response:\n\n\t" + strings.Join(smtpLines, "\n\t") + "\n"
	}

	deliverDSN(ctx, log, m, remoteMTA, secodeOpt, errmsg, smtpLines, false, &retryUntil, subject, message)
}

// We only queue DSNs for delivery failures for emails submitted by authenticated
// users. So we are delivering to local users. ../rfc/5321:1466
// ../rfc/5321:1494
// ../rfc/7208:490
func deliverDSN(ctx context.Context, log mlog.Log, m Msg, remoteMTA dsn.NameIP, secodeOpt, errmsg string, smtpLines []string, permanent bool, retryUntil *time.Time, subject, textBody string) {
	kind := "delayed delivery"
	if permanent {
		kind = "failure"
	}

	qlog := func(text string, err error) {
		log.Errorx("queue dsn: "+text+": sender will not be informed about dsn", err, slog.String("sender", m.Sender().XString(m.SMTPUTF8)), slog.String("kind", kind))
	}

	msgf, err := os.Open(m.MessagePath())
	if err != nil {
		qlog("opening queued message", err)
		return
	}
	msgr := store.FileMsgReader(m.MsgPrefix, msgf)
	defer func() {
		err := msgr.Close()
		log.Check(err, "closing message reader after queuing dsn")
	}()
	headers, err := message.ReadHeaders(bufio.NewReader(msgr))
	if err != nil {
		qlog("reading headers of queued message", err)
		return
	}

	var action dsn.Action
	var status string
	if permanent {
		status = "5."
		action = dsn.Failed
	} else {
		action = dsn.Delayed
		status = "4."
	}
	if secodeOpt != "" {
		status += secodeOpt
	} else {
		status += "0.0"
	}

	// ../rfc/3461:1329
	var smtpDiag string
	if len(smtpLines) > 0 {
		smtpDiag = "smtp; " + strings.Join(smtpLines, " ")
	}

	dsnMsg := &dsn.Message{
		SMTPUTF8:   m.SMTPUTF8,
		From:       smtp.Path{Localpart: "postmaster", IPDomain: dns.IPDomain{Domain: mox.Conf.Static.HostnameDomain}},
		To:         m.Sender(),
		Subject:    subject,
		MessageID:  mox.MessageIDGen(false),
		References: m.MessageID,
		TextBody:   textBody,

		ReportingMTA:         mox.Conf.Static.HostnameDomain.ASCII,
		ArrivalDate:          m.Queued,
		FutureReleaseRequest: m.FutureReleaseRequest,

		Recipients: []dsn.Recipient{
			{
				FinalRecipient:  m.Recipient(),
				Action:          action,
				Status:          status,
				StatusComment:   errmsg,
				RemoteMTA:       remoteMTA,
				DiagnosticCode:  smtpDiag,
				LastAttemptDate: *m.LastAttempt,
				WillRetryUntil:  retryUntil,
			},
		},

		Original: headers,
	}
	msgData, err := dsnMsg.Compose(log, m.SMTPUTF8)
	if err != nil {
		qlog("composing dsn", err)
		return
	}

	prefix := []byte("Return-Path: <" + dsnMsg.From.XString(m.SMTPUTF8) + ">\r\n" + "Delivered-To: " + m.Sender().XString(m.SMTPUTF8) + "\r\n")
	msgData = append(prefix, msgData...)

	mailbox := "Inbox"
	senderAccount := m.SenderAccount
	if m.IsDMARCReport {
		// senderAccount should already by postmaster, but doesn't hurt to ensure it.
		senderAccount = mox.Conf.Static.Postmaster.Account
	}
	acc, err := store.OpenAccount(log, senderAccount)
	if err != nil {
		acc, err = store.OpenAccount(log, mox.Conf.Static.Postmaster.Account)
		if err != nil {
			qlog("looking up postmaster account after sender account was not found", err)
			return
		}
		mailbox = mox.Conf.Static.Postmaster.Mailbox
	}
	defer func() {
		err := acc.Close()
		log.Check(err, "queue dsn: closing account", slog.String("sender", m.Sender().XString(m.SMTPUTF8)), slog.String("kind", kind))
	}()

	msgFile, err := store.CreateMessageTemp(log, "queue-dsn")
	if err != nil {
		qlog("creating temporary message file", err)
		return
	}
	defer store.CloseRemoveTempFile(log, msgFile, "dsn message")

	msgWriter := message.NewWriter(msgFile)
	if _, err := msgWriter.Write(msgData); err != nil {
		qlog("writing dsn message", err)
		return
	}

	msg := &store.Message{
		Received:  time.Now(),
		Size:      msgWriter.Size,
		MsgPrefix: []byte{},
		DSN:       true,
	}

	// If this is a DMARC report, deliver it as seen message to a submailbox of the
	// postmaster mailbox. We mark it as seen so it doesn't waste postmaster attention,
	// but we deliver them so they can be checked in case of problems.
	if m.IsDMARCReport {
		mailbox = fmt.Sprintf("%s/dmarc", mox.Conf.Static.Postmaster.Mailbox)
		msg.Seen = true
		metricDMARCReportFailure.Inc()
		log.Info("delivering dsn for failure to deliver outgoing dmarc report")
	}

	acc.WithWLock(func() {
		if err := acc.DeliverMailbox(log, mailbox, msg, msgFile); err != nil {
			qlog("delivering dsn to mailbox", err)
			return
		}
	})
}
