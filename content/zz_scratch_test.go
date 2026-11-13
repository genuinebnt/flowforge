package content

import (
	"fmt"
	"testing"

	"github.com/genuinebnt/flowforge/jsparse"
)

func TestScratch(t *testing.T) {
	src := `(self.webpackChunk=self.webpackChunk||[]).push([[179],{123:function(e,t,n){"use strict";var r=n(5);fetch("/api/v1/users/"+e.id,{method:"POST"});if(a){b()}else{c()}},456:(e,t)=>{e.exports={a:1,b:"x"}}}]);
function f(a,b){return a+b}const x=1,y=2;for(let i=0;i<3;i++){x++}`
	p, err := jsparse.Parse([]byte(src), jsparse.Options{Tolerant: true})
	fmt.Println(err, len(p.Diagnostics))
	fmt.Println(string(jsparse.Print(p, jsparse.PrintOptions{})))
	p2, _ := jsparse.Parse([]byte("var a = ; b(1)\nfoo("), jsparse.Options{Tolerant: true})
	fmt.Println(p2.Diagnostics)
	fmt.Printf("%q\n", jsparse.Print(p2, jsparse.PrintOptions{}))
}
