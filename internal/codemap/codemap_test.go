package codemap

import (
	"strings"
	"testing"
)

func names(syms []Symbol) string {
	var s []string
	for _, x := range syms {
		n := x.Name
		if x.Recv != "" {
			n = strings.TrimLeft(x.Recv, "*") + "." + n
		}
		s = append(s, n)
	}
	return strings.Join(s, " ")
}

func TestOutlineLanguages(t *testing.T) {
	cases := []struct{ file, src, want string }{
		{"a.go", `package a

type Store struct{ n int }
type Reader interface {
	Read(p []byte) (int, error)
}
const Max = 3
func New() *Store { return nil }
func (s *Store) Get(k string) (string, bool) {
	return "", false
}
`, "Store Reader Reader.Read Max New Store.Get"},
		{"a.py", `import os

class Cache:
    """doc"""
    def get(self, k):
        def inner():
            pass
        return 1

async def main():
    # def not_this():
    pass
`, "Cache Cache.get Cache.inner main"},
		{"a.ts", `/* class Hidden {} */
export interface Opts { a: number }
export default class Server extends Base {
  private port: number;
  constructor(p: number) {
    this.port = p;
  }
  async listen(host: string): Promise<void> {
    if (x) {
    }
  }
}
export const handler = async (req) => {
  return 1;
};
function helper() {}
`, "Opts Server Server.constructor Server.listen handler helper"},
		{"a.rs", `pub struct Point { x: i32 }
impl Display for Point {
    fn fmt(&self, f: &mut Formatter) -> Result {
        Ok(())
    }
}
pub(crate) async fn run() {}
`, "Point Point Point.fmt run"},
		{"A.java", `package x;
public class Account {
    private int balance;
    public void deposit(int amount) {
        if (amount > 0) {
        }
    }
    public static Account open(String name) throws IOException {
        return null;
    }
}
`, "Account Account.deposit Account.open"},
		{"a.c", `#include <stdio.h>
struct node {
    int v;
};
static int add(int a, int b)
{
    return a + b;
}
int main(void) {
    if (x) {
    }
    return 0;
}
`, "node add main"},
		{"a.rb", `module Billing
  class Invoice
    def total
    end
    def self.create!(x)
    end
  end
end
`, "Billing Billing.Invoice Invoice.total Invoice.create!"},
	}
	for _, c := range cases {
		if got := names(Outline(c.file, []byte(c.src))); got != c.want {
			t.Errorf("%s:\n got  %s\n want %s\n%s", c.file, got, c.want, Format(Outline(c.file, []byte(c.src))))
		}
	}
}

func TestMatchAndFormat(t *testing.T) {
	syms := Outline("a.go", []byte("package a\ntype T struct{}\nfunc (t *T) Run() error { return nil }\nfunc Run() {}\n"))
	var hits []int
	for _, s := range syms {
		if s.Match("T.Run") {
			hits = append(hits, s.Line)
		}
	}
	if len(hits) != 1 || hits[0] != 3 {
		t.Fatalf("T.Run hits %v", hits)
	}
	if f := Format(syms); !strings.Contains(f, "3: func (t *T) Run() error\n") {
		t.Fatalf("format: %q", f)
	}
	if Supported("x.md") || !Supported("x.TSX") {
		t.Fatal("Supported")
	}
}

func TestOutlineTrickySyntax(t *testing.T) {
	cases := []struct{ file, src, want string }{
		{"lt.rs", `struct X<'a> { s: &'a str }
impl<'a> Display for X<'a> {
    fn fmt(&self, f: &mut Formatter<'_>) -> Result {
        let c = '{';
        Ok(())
    }
}
fn after() {}
`, "X X X.fmt after"},
		{"doc.py", `def real():
    """
    def fake():
    class Fake:
    """
    return 1

class After:
    '''one-line doc'''
    def m(self): pass
`, "real After After.m"},
		{"tpl.ts", "const html = `\n  function notReal() {\n`;\n/* class Hidden {\n} */\nexport function real() {\n  return '{';\n}\n", "real"},
		{"crlf.java", "public class A {\r\n    public void run() {\r\n    }\r\n}\r\n", "A A.run"},
	}
	for _, c := range cases {
		if got := names(Outline(c.file, []byte(c.src))); got != c.want {
			t.Errorf("%s:\n got  %s\n want %s\n%s", c.file, got, c.want, Format(Outline(c.file, []byte(c.src))))
		}
	}
}
