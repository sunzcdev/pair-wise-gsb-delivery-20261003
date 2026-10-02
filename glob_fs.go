package sftp

import (
	"context"
	"os"
	"path"
)

// lstatContext is Lstat with a cancellable context.
func (c *Client) lstatContext(ctx context.Context, p string) (os.FileInfo, error) {
	id := c.nextID()
	typ, data, err := c.sendPacket(ctx, nil, &sshFxpLstatPacket{
		ID:   id,
		Path: p,
	})
	if err != nil {
		return nil, err
	}
	switch typ {
	case sshFxpAttrs:
		sid, data := unmarshalUint32(data)
		if sid != id {
			return nil, &unexpectedIDErr{id, sid}
		}
		attr, _, err := unmarshalAttrs(data)
		if err != nil {
			return nil, err
		}
		return fileInfoFromStat(attr, path.Base(p)), nil
	case sshFxpStatus:
		return nil, normaliseError(unmarshalStatus(id, data))
	default:
		return nil, unimplementedPacketErr(typ)
	}
}

func (c *Client) readlinkContext(ctx context.Context, p string) (string, error) {
	id := c.nextID()
	typ, data, err := c.sendPacket(ctx, nil, &sshFxpReadlinkPacket{
		ID:   id,
		Path: p,
	})
	if err != nil {
		return "", err
	}
	switch typ {
	case sshFxpName:
		sid, data := unmarshalUint32(data)
		if sid != id {
			return "", &unexpectedIDErr{id, sid}
		}
		count, data := unmarshalUint32(data)
		if count < 1 {
			return "", os.ErrInvalid
		}
		target, _ := unmarshalString(data)
		return target, nil
	case sshFxpStatus:
		return "", normaliseError(unmarshalStatus(id, data))
	default:
		return "", unimplementedPacketErr(typ)
	}
}

// resolveLink resolves a symlink to its final target using READLINK/STAT,
// bounded by maxHops. It returns the cleaned, slash-separated target path and
// reports whether that target is a directory.
func (g *GlobMatches) resolveLink(linkPath string) (string, bool, error) {
	current := linkPath
	for hops := 0; hops < g.cfg.maxHops; hops++ {
		target, err := g.client.readlinkContext(g.ctx(), current)
		if err != nil {
			return "", false, err
		}
		if !path.IsAbs(target) {
			target = path.Join(path.Dir(current), target)
		} else {
			target = path.Clean(target)
		}

		fi, err := g.client.lstatContext(g.ctx(), target)
		if err != nil {
			return "", false, err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			return target, fi.IsDir(), nil
		}
		current = target
	}
	// Hop budget exhausted: treat as a broken/looping link.
	return "", false, errSymlinkLoop
}
