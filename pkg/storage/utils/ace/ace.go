// Copyright 2018-2020 CERN
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// In applying this license, CERN does not waive the privileges and immunities
// granted to it by virtue of its status as an Intergovernmental Organization
// or submit itself to any jurisdiction.

package ace

import (
	"encoding/csv"
	"fmt"
	"strconv"
	"strings"

	userpb "github.com/cs3org/go-cs3apis/cs3/identity/user/v1beta1"
	provider "github.com/cs3org/go-cs3apis/cs3/storage/provider/v1beta1"
)

// ACE represents an Access Control Entry, mimicing NFSv4 ACLs
//
// The following is taken from the nfs4_acl man page,
// see https://linux.die.net/man/5/nfs4_acl:
// the extended attributes will look like this
// "user.oc.acl.<type>:<flags>:<principal>:<permissions>"
// - *type* will be limited to A for now
//     A: Allow - allow *principal* to perform actions requiring *permissions*
//   In the future we can use:
//     U: aUdit - log any attempted access by principal which requires
//                permissions.
//     L: aLarm - generate a system alarm at any attempted access by
//                principal which requires permissions
//   D for deny is not recommended
// - *flags* for now empty or g for group, no inheritance yet
//   - d directory-inherit - newly-created subdirectories will inherit the
//                           ACE.
//   - f file-inherit - newly-created files will inherit the ACE, minus its
//                      inheritance flags. Newly-created subdirectories
//                      will inherit the ACE; if directory-inherit is not
//                      also specified in the parent ACE, inherit-only will
//                      be added to the inherited ACE.
//   - n no-propagate-inherit - newly-created subdirectories will inherit
//                              the ACE, minus its inheritance flags.
//   - i inherit-only - the ACE is not considered in permissions checks,
//                      but it is heritable; however, the inherit-only
//                      flag is stripped from inherited ACEs.
// - *principal* a named user, group or special principal
//   - the oidc sub@iss maps nicely to this
//   - 'OWNER@', 'GROUP@', and 'EVERYONE@', which are, respectively, analogous to the POSIX user/group/other
// - *permissions*
//   - r read-data (files) / list-directory (directories)
//   - w write-data (files) / create-file (directories)
//   - a append-data (files) / create-subdirectory (directories)
//   - x execute (files) / change-directory (directories)
//   - d delete - delete the file/directory. Some servers will allow a delete to occur if either this permission is set in the file/directory or if the delete-child permission is set in its parent directory.
//   - D delete-child - remove a file or subdirectory from within the given directory (directories only)
//   - t read-attributes - read the attributes of the file/directory.
//   - T write-attributes - write the attributes of the file/directory.
//   - n read-named-attributes - read the named attributes of the file/directory.
//   - N write-named-attributes - write the named attributes of the file/directory.
//   - c read-ACL - read the file/directory NFSv4 ACL.
//   - C write-ACL - write the file/directory NFSv4 ACL.
//   - o write-owner - change ownership of the file/directory.
//   - y synchronize - allow clients to use synchronous I/O with the server.
// TODO implement OWNER@ as "user.oc.acl.A::OWNER@:rwaDxtTnNcCy"
// attribute names are limited to 255 chars by the linux kernel vfs, values to 64 kb
// ext3 extended attributes must fit inside a single filesystem block ... 4096 bytes
// that leaves us with "user.oc.acl.A::someonewithaslightlylongersubject@whateverissuer:rwaDxtTnNcCy" ~80 chars
// 4096/80 = 51 shares ... with luck we might move the actual permissions to the value, saving ~15 chars
// 4096/64 = 64 shares ... still meh ... we can do better by using ints instead of strings for principals
//   "user.oc.acl.u:100000" is pretty neat, but we can still do better: base64 encode the int
//   "user.oc.acl.u:6Jqg" but base64 always has at least 4 chars, maybe hex is better for smaller numbers
//   well use 4 chars in addition to the ace: "user.oc.acl.u:////" = 65535 -> 18 chars
// 4096/18 = 227 shares
// still ... ext attrs for this are not infinite scale ...
// so .. attach shares via fileid.
// <userhome>/metadata/<fileid>/shares, similar to <userhome>/files
// <userhome>/metadata/<fileid>/shares/u/<issuer>/<subject>/A:fdi:rwaDxtTnNcCy permissions as filename to keep them in the stat cache?
//
// whatever ... 50 shares is good enough. If more is needed we can delegate to the metadata
// if "user.oc.acl.M" is present look inside the metadata app.
// - if we cannot set an ace we might get an io error.
//   in that case convert all shares to metadata and try to set "user.oc.acl.m"
//
// what about metadata like share creator, share time, expiry?
// - creator is same as owner, but can be set
// - share date, or abbreviated st is a unix timestamp
// - expiry is a unix timestamp
// - can be put inside the value
// - we need to reorder the fields:
// "user.oc.acl.<u|g|o>:<principal>" -> "kv:t=<type>:f=<flags>:p=<permissions>:st=<share time>:c=<creator>:e=<expiry>:pw=<password>:n=<name>"
// "user.oc.acl.<u|g|o>:<principal>" -> "v1:<type>:<flags>:<permissions>:<share time>:<creator>:<expiry>:<password>:<name>"
// or the first byte determines the format
// 0x00 = key value
// 0x01 = v1 ...
type ACE struct {
	//NFSv4 acls
	_type       string // t
	flags       string // f
	principal   string // im key
	permissions string // p

	// sharing specific
	shareTime int    // s
	creator   string // c
	expires   int    // e
	password  string // w passWord TODO h = hash
	label     string // l
}

// FromGrant creates an ACE from a CS3 grant
func FromGrant(g *provider.Grant) *ACE {
	e := &ACE{
		_type:       "A",
		permissions: getACEPerm(g.Permissions),
		// TODO creator ...
	}
	if g.Grantee.Type == provider.GranteeType_GRANTEE_TYPE_GROUP {
		e.flags = "g"
		e.principal = "g:" + g.Grantee.Id.OpaqueId
	} else {
		e.principal = "u:" + g.Grantee.Id.OpaqueId
	}
	return e
}

// Principal returns the principal of the ACE, eg. `u:<userid>` or `g:<groupid>`
func (e *ACE) Principal() string {
	return e.principal
}

// Marshal renders a principal and byte[] that can be used to persist the ACE as an extended attribute
func (e *ACE) Marshal() (string, []byte) {
	// first byte will be replaced after converting to byte array
	val := fmt.Sprintf("_t=%s:f=%s:p=%s", e._type, e.flags, e.permissions)
	b := []byte(val)
	b[0] = 0 // indicate key value
	return e.principal, b
}

// Unmarshal parses a principal string and byte[] into an ACE
func Unmarshal(principal string, v []byte) (e *ACE, err error) {
	// first byte indicates type of value
	switch v[0] {
	case 0: // = ':' separated key=value pairs
		s := string(v[1:])
		if e, err = unmarshalKV(s); err == nil {
			e.principal = principal
		}
		// check consistency of Flags and principal type
		if strings.Contains(e.flags, "g") {
			if principal[:1] != "g" {
				return nil, fmt.Errorf("inconsistent ace: expected group")
			}
		} else {
			if principal[:1] != "u" {
				return nil, fmt.Errorf("inconsistent ace: expected user")
			}
		}
	default:
		return nil, fmt.Errorf("unknown ace encoding")
	}
	return
}

// Grant returns a CS3 grant
func (e *ACE) Grant() *provider.Grant {
	return &provider.Grant{
		Grantee: &provider.Grantee{
			Id:   &userpb.UserId{OpaqueId: e.principal},
			Type: e.granteeType(),
		},
		Permissions: e.grantPermissionSet(),
	}
}

// granteeType returns the CS3 grantee type
func (e *ACE) granteeType() provider.GranteeType {
	if strings.Contains(e.flags, "g") {
		return provider.GranteeType_GRANTEE_TYPE_GROUP
	}
	return provider.GranteeType_GRANTEE_TYPE_USER
}

// grantPermissionSet returns the set of CS3 resource permissions representing the ACE
func (e *ACE) grantPermissionSet() *provider.ResourcePermissions {
	p := &provider.ResourcePermissions{}
	// r
	if strings.Contains(e.permissions, "r") {
		p.Stat = true
		p.InitiateFileDownload = true
		p.ListContainer = true
	}
	// w
	if strings.Contains(e.permissions, "w") {
		p.InitiateFileUpload = true
		if p.InitiateFileDownload {
			p.Move = true
		}
	}
	//a
	if strings.Contains(e.permissions, "a") {
		// TODO append data to file permission?
		p.CreateContainer = true
	}
	//x
	//if strings.Contains(e.Permissions, "x") {
	// TODO execute file permission?
	// TODO change directory permission?
	//}
	//d
	if strings.Contains(e.permissions, "d") {
		p.Delete = true
	}
	//D ?

	// sharing
	if strings.Contains(e.permissions, "C") {
		p.AddGrant = true
		p.RemoveGrant = true
		p.UpdateGrant = true
	}
	if strings.Contains(e.permissions, "c") {
		p.ListGrants = true
	}

	// trash
	if strings.Contains(e.permissions, "u") { // u = undelete
		p.ListRecycle = true
	}
	if strings.Contains(e.permissions, "U") {
		p.RestoreRecycleItem = true
	}
	if strings.Contains(e.permissions, "P") {
		p.PurgeRecycle = true
	}

	// versions
	if strings.Contains(e.permissions, "v") {
		p.ListFileVersions = true
	}
	if strings.Contains(e.permissions, "V") {
		p.RestoreFileVersion = true
	}

	// ?
	// TODO GetPath
	if strings.Contains(e.permissions, "q") {
		p.GetQuota = true
	}
	// TODO set quota permission?
	return p
}

func unmarshalKV(s string) (*ACE, error) {
	e := &ACE{}
	r := csv.NewReader(strings.NewReader(s))
	r.Comma = ':'
	r.Comment = 0
	r.FieldsPerRecord = -1
	r.LazyQuotes = false
	r.TrimLeadingSpace = false
	records, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(records) != 1 {
		return nil, fmt.Errorf("more than one row of ace kvs")
	}
	for i := range records[0] {
		kv := strings.Split(records[0][i], "=")
		switch kv[0] {
		case "t":
			e._type = kv[1]
		case "f":
			e.flags = kv[1]
		case "p":
			e.permissions = kv[1]
		case "s":
			v, err := strconv.Atoi(kv[1])
			if err != nil {
				return nil, err
			}
			e.shareTime = v
		case "c":
			e.creator = kv[1]
		case "e":
			v, err := strconv.Atoi(kv[1])
			if err != nil {
				return nil, err
			}
			e.expires = v
		case "w":
			e.password = kv[1]
		case "l":
			e.label = kv[1]
			// TODO default ... log unknown keys? or add as opaque? hm we need that for tagged shares ...
		}
	}
	return e, nil
}

func getACEPerm(set *provider.ResourcePermissions) string {
	var b strings.Builder

	if set.Stat || set.InitiateFileDownload || set.ListContainer {
		b.WriteString("r")
	}
	if set.InitiateFileUpload || set.Move {
		b.WriteString("w")
	}
	if set.CreateContainer {
		b.WriteString("a")
	}
	if set.Delete {
		b.WriteString("d")
	}

	// sharing
	if set.AddGrant || set.RemoveGrant || set.UpdateGrant {
		b.WriteString("C")
	}
	if set.ListGrants {
		b.WriteString("c")
	}

	// trash
	if set.ListRecycle {
		b.WriteString("u")
	}
	if set.RestoreRecycleItem {
		b.WriteString("U")
	}
	if set.PurgeRecycle {
		b.WriteString("P")
	}

	// versions
	if set.ListFileVersions {
		b.WriteString("v")
	}
	if set.RestoreFileVersion {
		b.WriteString("V")
	}

	// quota
	if set.GetQuota {
		b.WriteString("q")
	}
	// TODO set quota permission?
	// TODO GetPath
	return b.String()
}
