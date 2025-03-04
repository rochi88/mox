package imapserver

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/mjl-/bstore"

	"github.com/mjl-/mox/store"
)

// Changed during tests.
var metadataMaxKeys = 1000
var metadataMaxSize = 1000 * 1000

// Metadata errata:
// ../rfc/5464:183 ../rfc/5464-eid1691
// ../rfc/5464:564 ../rfc/5464-eid1692
// ../rfc/5464:494 ../rfc/5464-eid2785 ../rfc/5464-eid2786
// ../rfc/5464:698 ../rfc/5464-eid3868

// Note: We do not tie the special-use mailbox flags to a (synthetic) private
// per-mailbox annotation. ../rfc/6154:303

// For registration of names, see https://www.iana.org/assignments/imap-metadata/imap-metadata.xhtml

// Get metadata annotations, per mailbox or globally.
//
// State: Authenticated and selected.
func (c *conn) cmdGetmetadata(tag, cmd string, p *parser) {
	// Command: ../rfc/5464:412

	// Request syntax: ../rfc/5464:792

	p.xspace()
	var optMaxSize int64 = -1
	var optDepth string
	if p.take("(") {
		for {
			if p.take("MAXSIZE") {
				// ../rfc/5464:804
				p.xspace()
				v := p.xnumber()
				if optMaxSize >= 0 {
					p.xerrorf("only a single maxsize option accepted")
				}
				optMaxSize = int64(v)
			} else if p.take("DEPTH") {
				// ../rfc/5464:823
				p.xspace()
				s := p.xtakelist("0", "1", "INFINITY")
				if optDepth != "" {
					p.xerrorf("only single depth option accepted")
				}
				optDepth = s
			} else {
				// ../rfc/5464:800 We are not doing anything further parsing for future extensions.
				p.xerrorf("unknown option for getmetadata, expected maxsize or depth")
			}

			if p.take(")") {
				break
			}
			p.xspace()
		}
		p.xspace()
	}
	mailboxName := p.xmailbox()
	if mailboxName != "" {
		mailboxName = xcheckmailboxname(mailboxName, true)
	}
	p.xspace()
	// Entries ../rfc/5464:768
	entryNames := map[string]struct{}{}
	if p.take("(") {
		for {
			s := p.xmetadataKey()
			entryNames[s] = struct{}{}
			if p.take(")") {
				break
			}
			p.xtake(" ")
		}
	} else {
		s := p.xmetadataKey()
		entryNames[s] = struct{}{}
	}
	p.xempty()

	var annotations []store.Annotation
	longentries := -1 // Size of largest value skipped due to optMaxSize. ../rfc/5464:482

	c.account.WithRLock(func() {
		c.xdbread(func(tx *bstore.Tx) {
			q := bstore.QueryTx[store.Annotation](tx)
			if mailboxName == "" {
				q.FilterEqual("MailboxID", 0)
			} else {
				mb := c.xmailbox(tx, mailboxName, "TRYCREATE")
				q.FilterNonzero(store.Annotation{MailboxID: mb.ID})
			}

			err := q.ForEach(func(a store.Annotation) error {
				// ../rfc/5464:516
				switch optDepth {
				case "", "0":
					if _, ok := entryNames[a.Key]; !ok {
						return nil
					}
				case "1", "INFINITY":
					// Go through all keys, matching depth.
					if _, ok := entryNames[a.Key]; ok {
						break
					}
					var match bool
					for s := range entryNames {
						prefix := s
						if s != "/" {
							prefix += "/"
						}
						if !strings.HasPrefix(a.Key, prefix) {
							continue
						}
						if optDepth == "INFINITY" {
							match = true
							break
						}
						suffix := a.Key[len(prefix):]
						t := strings.SplitN(suffix, "/", 2)
						if len(t) == 1 {
							match = true
							break
						}
					}
					if !match {
						return nil
					}
				default:
					xcheckf(fmt.Errorf("%q", optDepth), "missing case for depth")
				}

				if optMaxSize >= 0 && int64(len(a.Value)) > optMaxSize {
					longentries = max(longentries, len(a.Value))
				} else {
					annotations = append(annotations, a)
				}
				return nil
			})
			xcheckf(err, "looking up annotations")
		})
	})

	// Response syntax: ../rfc/5464:807 ../rfc/5464:778
	// We can only send untagged responses when we have any matches.
	if len(annotations) > 0 {
		fmt.Fprintf(c.bw, "* METADATA %s (", astring(mailboxName).pack(c))
		for i, a := range annotations {
			if i > 0 {
				fmt.Fprint(c.bw, " ")
			}
			astring(a.Key).writeTo(c, c.bw)
			fmt.Fprint(c.bw, " ")
			if a.IsString {
				string0(string(a.Value)).writeTo(c, c.bw)
			} else {
				v := readerSizeSyncliteral{bytes.NewReader(a.Value), int64(len(a.Value)), true}
				v.writeTo(c, c.bw)
			}
		}
		c.bwritelinef(")")
	}

	if longentries >= 0 {
		c.bwritelinef("%s OK [METADATA LONGENTRIES %d] getmetadata done", tag, longentries)
	} else {
		c.ok(tag, cmd)
	}
}

// Set metadata annotation, per mailbox or globally.
//
// We only implement private annotations, not shared annotations. We don't
// currently have a mechanism for determining if the user should have access.
//
// State: Authenticated and selected.
func (c *conn) cmdSetmetadata(tag, cmd string, p *parser) {
	// Command: ../rfc/5464:547

	// Request syntax: ../rfc/5464:826

	p.xspace()
	mailboxName := p.xmailbox()
	// Empty name means a global (per-account) annotation, not for a mailbox.
	if mailboxName != "" {
		mailboxName = xcheckmailboxname(mailboxName, true)
	}
	p.xspace()
	p.xtake("(")
	var l []store.Annotation
	for {
		key, isString, value := p.xmetadataKeyValue()
		l = append(l, store.Annotation{Key: key, IsString: isString, Value: value})
		if p.take(")") {
			break
		}
		p.xspace()
	}
	p.xempty()

	// Additional checks on entry names.
	for _, a := range l {
		// We only allow /private/* entry names, so check early and fail if we see anything
		// else (the only other option is /shared/* at this moment).
		// ../rfc/5464:217
		if !strings.HasPrefix(a.Key, "/private/") {
			// ../rfc/5464:346
			xuserErrorf("only /private/* entry names allowed")
		}

		// We also enforce that /private/vendor/ is followed by at least 2 elements.
		// ../rfc/5464:234
		if a.Key == "/private/vendor" || strings.HasPrefix(a.Key, "/private/vendor/") {
			t := strings.SplitN(a.Key[1:], "/", 4)
			if len(t) < 4 {
				xuserErrorf("entry names starting with /private/vendor must have at least 4 components")
			}
		}
	}

	// Store the annotations, possibly removing/inserting/updating them.
	c.account.WithWLock(func() {
		var changes []store.Change

		c.xdbwrite(func(tx *bstore.Tx) {
			var mb store.Mailbox // mb.ID as 0 is used in query below.
			if mailboxName != "" {
				mb = c.xmailbox(tx, mailboxName, "TRYCREATE")
			}

			for _, a := range l {
				q := bstore.QueryTx[store.Annotation](tx)
				q.FilterNonzero(store.Annotation{Key: a.Key})
				q.FilterEqual("MailboxID", mb.ID) // Can be zero.

				// Nil means remove. ../rfc/5464:579
				if a.Value == nil {
					var deleted []store.Annotation
					q.Gather(&deleted)
					_, err := q.Delete()
					xcheckf(err, "deleting annotation")
					for _, oa := range deleted {
						changes = append(changes, oa.Change(mailboxName))
					}
					continue
				}

				a.MailboxID = mb.ID

				oa, err := q.Get()
				if err == bstore.ErrAbsent {
					err = tx.Insert(&a)
					xcheckf(err, "inserting annotation")
					changes = append(changes, a.Change(mailboxName))
					continue
				}
				xcheckf(err, "looking up existing annotation for entry name")
				if oa.IsString != a.IsString || (oa.Value == nil) != (a.Value == nil) || !bytes.Equal(oa.Value, a.Value) {
					changes = append(changes, a.Change(mailboxName))
				}
				oa.Value = a.Value
				err = tx.Update(&oa)
				xcheckf(err, "updating metadata annotation")
			}

			// Check for total size. We allow a total of 1000 entries, with total capacity of 1MB.
			// ../rfc/5464:383
			var n int
			var size int
			err := bstore.QueryTx[store.Annotation](tx).ForEach(func(a store.Annotation) error {
				n++
				if n > metadataMaxKeys {
					// ../rfc/5464:590
					xusercodeErrorf("METADATA TOOMANY", "too many metadata entries, 1000 allowed in total")
				}
				size += len(a.Key) + len(a.Value)
				if size > metadataMaxSize {
					// ../rfc/5464:585 We only have a max total size limit, not per entry. We'll
					// mention the max total size.
					xusercodeErrorf(fmt.Sprintf("METADATA MAXSIZE %d", metadataMaxSize), "metadata entry values too large, total maximum size is 1MB")
				}
				return nil
			})
			xcheckf(err, "checking metadata annotation size")
		})

		c.broadcast(changes)
	})

	c.ok(tag, cmd)
}
