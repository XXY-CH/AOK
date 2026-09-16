//go:build !linux || !arm64

package kernelbridge

func Bootstrap() (*Root, error)                                  { return nil, ErrUnsupported }
func (r *Root) Create(CapabilitySpec) (*Capability, error)       { return nil, ErrUnsupported }
func (c *Capability) Derive(CapabilitySpec) (*Capability, error) { return nil, ErrUnsupported }
func (c *Capability) Revoke() error                              { return ErrUnsupported }
func (c *Capability) Check() error                               { return ErrUnsupported }
func (c *Capability) Session() (*Client, *Backend, error)        { return nil, nil, ErrUnsupported }
func (c *Client) Submit(Record) error                            { return ErrUnsupported }
func (c *Client) Result() (Record, error)                        { return Record{}, ErrUnsupported }
func (c *Client) Export() (Record, error)                        { return Record{}, ErrUnsupported }
func (c *Client) Cancel() error                                  { return ErrUnsupported }
func (b *Backend) Take() (Record, error)                         { return Record{}, ErrUnsupported }
func (b *Backend) Complete(Record) error                         { return ErrUnsupported }
